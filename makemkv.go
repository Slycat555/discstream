package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MakeMKV robot-mode (-r) attribute ids, from makemkv's apdefs.h.
const (
	apDuration       = 9
	apDiskSizeBytes  = 11
	apOutputFileName = 27
)

type mkvTitle struct {
	id       int
	duration time.Duration
	size     int64
	file     string
}

// findMakeMKV returns the command prefix used to run makemkvcon: $MAKEMKVCON,
// makemkvcon on PATH, or the MakeMKV Flatpak (granted access to workDir).
func findMakeMKV(workDir string) ([]string, error) {
	if c := os.Getenv("MAKEMKVCON"); c != "" {
		return strings.Fields(c), nil
	}
	if p, err := exec.LookPath("makemkvcon"); err == nil {
		return []string{p}, nil
	}
	if fp, err := exec.LookPath("flatpak"); err == nil {
		if exec.Command(fp, "info", "com.makemkv.MakeMKV").Run() == nil {
			return []string{fp, "run", "--filesystem=" + workDir, "--command=makemkvcon", "com.makemkv.MakeMKV"}, nil
		}
	}
	return nil, errors.New("MakeMKV is needed for this disc, but makemkvcon was not found.\n" +
		"Install MakeMKV (https://www.makemkv.com/forum/viewtopic.php?f=3&t=224, or the Flatpak\n" +
		"com.makemkv.MakeMKV), or point MAKEMKVCON at the makemkvcon binary")
}

func mkvSource(src source) string {
	switch src.kind {
	case srcImage:
		return "iso:" + src.path
	case srcFolder:
		return "file:" + src.path
	}
	return "dev:" + src.path
}

// runRobot runs makemkvcon in robot mode, relaying its messages and calling
// onLine for every other output line.
func runRobot(ctx context.Context, mkv []string, args []string, onLine func(kind string, fields []string)) error {
	full := append(append([]string{}, mkv[1:]...), args...)
	cmd := exec.CommandContext(ctx, mkv[0], full...)
	// Own process group, so stopping also reaches helpers such as the
	// Flatpak sandbox. makemkvcon blocked opening its FIFO may ignore
	// SIGINT, so the group is killed if it hasn't exited after the delay.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		syscall.Kill(-pgid, syscall.SIGINT)
		time.AfterFunc(3*time.Second, func() { syscall.Kill(-pgid, syscall.SIGKILL) })
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	// Read through an io.Pipe rather than StdoutPipe so Wait (and WaitDelay)
	// isn't held up by a leftover child keeping stdout open.
	out, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	var lastMsg string
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			kind, rest, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			fields := splitRobot(rest)
			if kind == "MSG" && len(fields) >= 4 {
				lastMsg = fields[3]
				log.Print("makemkv: " + lastMsg)
				continue
			}
			if onLine != nil {
				onLine(kind, fields)
			}
		}
		io.Copy(io.Discard, out)
	}()
	err := cmd.Wait()
	pw.Close()
	<-scanned
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("makemkvcon failed: %v (last message: %q)", err, lastMsg)
	}
	return ctx.Err()
}

// splitRobot splits a robot-mode line into fields. Quoted fields may contain
// commas and backslash-escaped or doubled quotes.
func splitRobot(s string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case inQuote && c == '"' && i+1 < len(s) && s[i+1] == '"':
			i++
			cur.WriteByte('"')
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(fields, cur.String())
}

func mkvInfo(ctx context.Context, mkv []string, src source, minLength int) ([]mkvTitle, error) {
	titles := map[int]*mkvTitle{}
	var order []int
	err := runRobot(ctx, mkv, []string{"-r", "--noscan", fmt.Sprintf("--minlength=%d", minLength),
		"info", mkvSource(src)}, func(kind string, f []string) {
		if kind != "TINFO" || len(f) < 4 {
			return
		}
		id, err1 := strconv.Atoi(f[0])
		attr, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return
		}
		t := titles[id]
		if t == nil {
			t = &mkvTitle{id: id}
			titles[id] = t
			order = append(order, id)
		}
		switch attr {
		case apDuration:
			t.duration = parseHMS(f[3])
		case apDiskSizeBytes:
			t.size, _ = strconv.ParseInt(f[3], 10, 64)
		case apOutputFileName:
			t.file = f[3]
		}
	})
	if err != nil {
		return nil, err
	}
	var list []mkvTitle
	for _, id := range order {
		if titles[id].file != "" {
			list = append(list, *titles[id])
		}
	}
	return list, nil
}

func parseHMS(s string) time.Duration {
	var d time.Duration
	for _, p := range strings.Split(s, ":") {
		n, _ := strconv.Atoi(p)
		d = d*60 + time.Duration(n)
	}
	return d * time.Second
}

// streamMakeMKV has MakeMKV decrypt and remux the chosen title into a FIFO
// that ffmpeg reads. MakeMKV names its output file itself, so the FIFO is
// created under the exact name that `info` reported for the title.
func streamMakeMKV(ctx context.Context, o options, t tools, mkv []string, src source, workDir, hlsDir string, pick mkvTitle, gen int) error {
	fifoDir := filepath.Join(workDir, "makemkv")
	if err := os.MkdirAll(fifoDir, 0o755); err != nil {
		return err
	}
	fifo := filepath.Join(fifoDir, pick.file)
	os.Remove(fifo) // left over from an earlier play of this title
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return fmt.Errorf("mkfifo: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mkvDone := make(chan error, 1)
	go func() {
		err := runRobot(ctx, mkv, []string{"-r", "--noscan", fmt.Sprintf("--minlength=%d", o.minLength),
			"mkv", mkvSource(src), strconv.Itoa(pick.id), fifoDir}, nil)
		if err != nil {
			// ffmpeg may still be blocked opening the FIFO; stop it.
			cancel()
		}
		mkvDone <- err
	}()

	args := append(inputArgs(o), "-f", "matroska", "-i", fifo)
	ffErr := runFFmpeg(ctx, t, append(args, outputArgs(o, false, hlsDir, gen)...), nil)
	cancel() // stop MakeMKV if ffmpeg finished or failed first
	mkvErr := <-mkvDone
	if mkvErr != nil && !errors.Is(mkvErr, context.Canceled) {
		return mkvErr
	}
	return ffErr
}
