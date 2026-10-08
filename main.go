// discstream streams the disc in an optical drive to the local network as HLS.
//
// Unencrypted discs are read directly by ffmpeg. Encrypted discs (DVD CSS,
// Blu-ray AACS/BD+) are decrypted by MakeMKV, which writes into a FIFO that
// ffmpeg reads from. Everything is re-timed onto a single clock so audio and
// video stay in sync.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

var version = "dev"

type options struct {
	device      string
	bind        string
	port        int
	title       int
	audio       int
	makemkvMode string
	minLength   int
	preset      string
	crf         int
	maxRate     string
	maxHeight   int
	copyVideo   bool
	segment     int
	window      int
	wait        bool
	exitDone    bool
	realtime    bool
	tray        bool
	workRoot    string
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.device, "device", "/dev/sr0", "optical drive device (or an ISO file / VIDEO_TS / BDMV folder)")
	flag.StringVar(&o.bind, "bind", "0.0.0.0", "address to listen on")
	flag.IntVar(&o.port, "port", 8080, "HTTP port")
	flag.IntVar(&o.title, "title", 0, "title to play (0 = auto: longest). DVD: title number, Blu-ray: playlist number, MakeMKV: title index+1, CD: start track")
	flag.IntVar(&o.audio, "audio", 0, "audio track index (0 = first audio stream)")
	flag.StringVar(&o.makemkvMode, "makemkv", "auto", "when to use MakeMKV: auto (only for encrypted discs), always, never")
	flag.IntVar(&o.minLength, "minlength", 300, "MakeMKV: ignore titles shorter than this many seconds")
	flag.StringVar(&o.preset, "preset", "veryfast", "x264 preset")
	flag.IntVar(&o.crf, "crf", 21, "x264 CRF quality (lower = better)")
	flag.StringVar(&o.maxRate, "maxrate", "8M", "maximum video bitrate")
	flag.IntVar(&o.maxHeight, "max-height", 1080, "downscale video taller than this")
	flag.BoolVar(&o.copyVideo, "copy-video", false, "pass the video through without re-encoding (source must be H.264)")
	flag.IntVar(&o.segment, "segment", 4, "HLS segment length in seconds")
	flag.IntVar(&o.window, "window", 0, "keep only the last N segments (0 = keep everything so viewers can seek back)")
	flag.BoolVar(&o.wait, "wait", true, "wait for a disc to be inserted")
	flag.BoolVar(&o.realtime, "realtime", false, "read the disc at playback speed instead of caching ahead (less CPU and disk, but no seeking forward)")
	flag.BoolVar(&o.tray, "tray", true, "show a system tray icon (Open player, Quit)")
	flag.BoolVar(&o.exitDone, "exit", false, "exit when the disc has finished playing instead of continuing to serve")
	flag.StringVar(&o.workRoot, "workdir", "", "directory for temporary stream segments (default: ~/.cache/discstream)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "discstream %s - stream an optical disc over the local network\n\nUsage: %s [options]\n\n", version, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	if o.window > 0 && !o.realtime {
		// Caching ahead would delete segments before anyone watched them.
		log.Print("-window implies -realtime")
		o.realtime = true
	}
	switch o.makemkvMode {
	case "auto", "always", "never":
	default:
		log.Fatalf("-makemkv must be auto, always or never")
	}
	return o
}

func main() {
	log.SetFlags(log.Ltime)
	o := parseFlags()

	tools, err := findTools()
	if err != nil {
		log.Fatal(err)
	}

	if o.workRoot == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		o.workRoot = filepath.Join(cache, "discstream")
	}
	if err := os.MkdirAll(o.workRoot, 0o755); err != nil {
		log.Fatal(err)
	}
	// Also log to a file: when started from the app menu there is no terminal.
	logPath := filepath.Join(o.workRoot, "discstream.log")
	if f, err := os.Create(logPath); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
		defer f.Close()
	} else {
		logPath = ""
	}
	removeStaleSessions(o.workRoot)
	workDir, err := os.MkdirTemp(o.workRoot, "session-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(workDir)
	os.WriteFile(filepath.Join(workDir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	hlsDir := filepath.Join(workDir, "hls")
	if err := os.Mkdir(hlsDir, 0o755); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	st := &status{State: "waiting"}
	p := newPlayer(ctx, o, tools, workDir, hlsDir, st)
	portExplicit := false
	flag.Visit(func(f *flag.Flag) { portExplicit = portExplicit || f.Name == "port" })
	srv, port, err := startServer(o, portExplicit, st, p)
	if err != nil {
		log.Print(err)
		os.RemoveAll(workDir)
		os.Exit(1)
	}
	o.port = port
	defer srv.Close()
	printURLs(o)
	if o.tray {
		if trayAvailable() {
			endTray := runTray(o, st, p, logPath, stop)
			defer endTray()
		} else {
			log.Print("no system tray found; running without a tray icon (press Ctrl+C to quit)")
		}
	}

	if err := p.prepare(); err != nil {
		if ctx.Err() != nil {
			return
		}
		st.set("error", err.Error())
		log.Printf("error: %v", err)
		if o.exitDone {
			os.RemoveAll(workDir)
			os.Exit(1)
		}
		log.Print("press Ctrl+C to quit")
		<-ctx.Done()
		return
	}
	if err := p.play(p.defaultTitle()); err != nil {
		log.Print(err)
	}

	if o.exitDone {
		select {
		case err = <-p.finished:
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}
	log.Print("stopping")
	p.stop()
	if err != nil {
		os.RemoveAll(workDir)
		os.Exit(1)
	}
}

// removeStaleSessions deletes segment directories left behind by instances
// that were killed before they could clean up.
func removeStaleSessions(root string) {
	dirs, _ := filepath.Glob(filepath.Join(root, "session-*"))
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "pid"))
		if err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil && syscall.Kill(pid, 0) == nil {
				continue // still running
			}
		}
		os.RemoveAll(d)
	}
}
