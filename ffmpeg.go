package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type tools struct{ ffmpeg, ffprobe string }

// findTools prefers the ffmpeg bundled in the AppImage (or next to the
// binary) and falls back to the one on PATH.
func findTools() (tools, error) {
	var dirs []string
	if appdir := os.Getenv("APPDIR"); appdir != "" {
		dirs = append(dirs, filepath.Join(appdir, "usr", "bin"))
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	find := func(name string) (string, error) {
		for _, d := range dirs {
			p := filepath.Join(d, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return p, nil
			}
		}
		return exec.LookPath(name)
	}
	ff, err := find("ffmpeg")
	if err != nil {
		return tools{}, errors.New("ffmpeg not found")
	}
	fp, err := find("ffprobe")
	if err != nil {
		return tools{}, errors.New("ffprobe not found")
	}
	t := tools{ff, fp}
	t.checkCapabilities()
	return t, nil
}

// checkCapabilities warns when ffmpeg can't read discs directly (distro
// builds often lack libbluray/libdvdnav); such discs then go through MakeMKV.
func (t tools) checkCapabilities() {
	protos, _ := exec.Command(t.ffmpeg, "-hide_banner", "-protocols").Output()
	demux, _ := exec.Command(t.ffmpeg, "-hide_banner", "-demuxers").Output()
	if !bytes.Contains(protos, []byte("bluray")) || !bytes.Contains(demux, []byte("dvdvideo")) {
		log.Printf("warning: %s was built without Blu-ray/DVD-Video support; "+
			"discs will be detected less reliably and read through MakeMKV", t.ffmpeg)
	}
}

// inputArgs are the options shared by every source. genpts fills in missing
// timestamps so both streams share one clock. By default the disc is read
// as fast as ffmpeg can encode, caching ahead of the viewers; with realtime
// it is paced to playback speed (after a short burst so playback starts
// quickly).
func inputArgs(o options) []string {
	a := []string{
		"-hide_banner", "-nostdin", "-loglevel", "info", "-stats_period", "15",
		"-fflags", "+genpts+discardcorrupt",
		"-thread_queue_size", "1024",
	}
	if o.realtime {
		a = append(a, "-readrate", "1", "-readrate_initial_burst", "10")
	}
	return a
}

// outputArgs encodes to H.264/AAC and writes an HLS playlist.
//
// Sync: video is forced to constant frame rate (frames are duplicated or
// dropped to follow the timestamps) and audio is resampled with async
// compensation, so gaps and timestamp jumps at cell/clip boundaries are
// filled with silence instead of shifting audio against video. A late audio
// start is left to the timestamps: padding it with first_pts=0 counted the
// gap twice on Blu-ray (MPEG-TS) input and made audio ~0.5 s late.
func outputArgs(o options, audioOnly bool, hlsDir string, gen int) []string {
	var a []string
	if audioOnly {
		a = append(a, "-map", "0:a:0", "-vn")
	} else {
		a = append(a, "-map", "0:v:0", "-map", fmt.Sprintf("0:a:%d", o.audio))
		if o.copyVideo {
			a = append(a, "-c:v", "copy")
		} else {
			vf := "bwdif=mode=send_frame:deint=interlaced," +
				"scale=w='trunc(iw*sar/2)*2':h=ih,setsar=1," +
				fmt.Sprintf("scale=w=-2:h='min(ih,%d)',format=yuv420p", o.maxHeight)
			a = append(a,
				"-vf", vf,
				"-c:v", "libx264", "-preset", o.preset, "-crf", strconv.Itoa(o.crf),
				"-maxrate", o.maxRate, "-bufsize", doubleRate(o.maxRate),
				"-profile:v", "high",
				"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", o.segment),
				"-sc_threshold", "0",
				"-fps_mode", "cfr",
			)
		}
	}
	a = append(a,
		"-af", "aresample=async=1:min_hard_comp=0.100",
		"-c:a", "aac", "-b:a", "192k", "-ac", "2", "-ar", "48000",
		"-sn", "-dn",
		"-max_muxing_queue_size", "4096",
		"-f", "hls",
		"-hls_time", strconv.Itoa(o.segment),
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(hlsDir, fmt.Sprintf("g%d-%%05d.ts", gen)),
	)
	if o.window > 0 {
		a = append(a, "-hls_list_size", strconv.Itoa(o.window), "-hls_flags", "independent_segments+delete_segments")
	} else {
		a = append(a, "-hls_list_size", "0", "-hls_playlist_type", "event", "-hls_flags", "independent_segments")
	}
	return append(a, filepath.Join(hlsDir, "index.m3u8"))
}

func doubleRate(r string) string {
	num := strings.TrimRight(r, "kKmMgG")
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return r
	}
	return strconv.FormatFloat(v*2, 'f', -1, 64) + r[len(num):]
}

// runFFmpeg runs ffmpeg and relays the useful parts of its log.
func runFFmpeg(ctx context.Context, t tools, args []string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, t.ffmpeg, args...)
	cmd.Env = blurayEnv()
	cmd.Stdin = stdin
	// Interrupt first so ffmpeg can finish the playlist, but don't wait long:
	// a stopped stream's segments are discarded anyway.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	tail := relayFFmpegLog(stderr)
	err = cmd.Wait()
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("ffmpeg failed: %v\n%s", err, strings.Join(tail(), "\n"))
	}
	return nil
}

// relayFFmpegLog prints the input stream list, errors and periodic
// progress. The returned function yields the last lines for error reports.
func relayFFmpegLog(r io.Reader) func() []string {
	done := make(chan struct{})
	var last []string
	go func() {
		defer close(done)
		sc := bufio.NewScanner(r)
		sc.Split(scanCRLF)
		inInput := false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			last = append(last, line)
			if len(last) > 15 {
				last = last[1:]
			}
			switch {
			case strings.HasPrefix(line, "Input #"):
				inInput = true
				log.Print("ffmpeg: " + line)
			case strings.HasPrefix(line, "Output #"), strings.HasPrefix(line, "Stream mapping"):
				inInput = false
			case inInput && (strings.HasPrefix(line, "Stream #") || strings.HasPrefix(line, "Duration:")):
				log.Print("ffmpeg:   " + line)
			case strings.HasPrefix(line, "frame=") || strings.HasPrefix(line, "size="):
				log.Print("progress: " + line)
			case strings.Contains(line, "rror") && !strings.Contains(line, "Immediate exit requested"):
				log.Print("ffmpeg: " + line)
			}
		}
	}()
	return func() []string { <-done; return last }
}

func scanCRLF(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func streamDirect(ctx context.Context, o options, t tools, src source, d discInfo, hlsDir string, title, gen int) error {
	args := inputArgs(o)
	switch d.kind {
	case kindDVD:
		args = append(args, "-f", "dvdvideo", "-title", strconv.Itoa(title), "-i", src.path)
	case kindBluRay:
		args = append(args, "-playlist", strconv.Itoa(title), "-i", "bluray:"+src.path)
	default:
		return fmt.Errorf("cannot play %s directly", d.kind)
	}
	return runFFmpeg(ctx, t, append(args, outputArgs(o, false, hlsDir, gen)...), nil)
}

// listDVDTitles probes titles in order until one is missing; durations
// come from the IFO program chains so this is fast.
func listDVDTitles(ctx context.Context, t tools, path string) ([]title, error) {
	var titles []title
	for n := 1; n <= 99; n++ {
		pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out, err := exec.CommandContext(pctx, t.ffprobe, "-v", "quiet", "-f", "dvdvideo",
			"-title", strconv.Itoa(n), "-show_entries", "format=duration",
			"-of", "default=nw=1:nk=1", path).Output()
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			break
		}
		dur, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		titles = append(titles, title{ID: n, Name: fmt.Sprintf("Title %d", n), Duration: dur})
	}
	if len(titles) == 0 {
		return nil, errors.New("no playable DVD titles found")
	}
	return titles, nil
}

var playlistLine = regexp.MustCompile(`playlist (\d+)\.mpls \((\d+):(\d+):(\d+)\)`)

// listBlurayPlaylists returns the playlists libbluray considers usable
// (it already hides very short and duplicate ones).
func listBlurayPlaylists(ctx context.Context, t tools, path string) ([]title, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.ffprobe, "-hide_banner", "-v", "info", "bluray:"+path)
	cmd.Env = blurayEnv()
	out, _ := cmd.CombinedOutput()
	var titles []title
	for _, m := range playlistLine.FindAllStringSubmatch(string(out), -1) {
		n, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		se, _ := strconv.Atoi(m[4])
		titles = append(titles, title{ID: n, Name: fmt.Sprintf("Playlist %05d", n), Duration: float64(h*3600 + mi*60 + se)})
	}
	if len(titles) == 0 {
		return nil, errors.New("no playable Blu-ray playlists found")
	}
	return titles, nil
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func streamAudioCD(ctx context.Context, o options, t tools, src source, hlsDir string, tracks []cdTrack, gen int) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(readCDDA(ctx, src.path, tracks, pw)) }()
	defer pr.Close()

	args := append(inputArgs(o), "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0")
	return runFFmpeg(ctx, t, append(args, outputArgs(o, true, hlsDir, gen)...), pr)
}
