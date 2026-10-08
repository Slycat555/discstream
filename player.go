package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// title is one selectable item: a DVD title, Blu-ray playlist, MakeMKV title
// or CD track.
type title struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Duration float64 `json:"duration"` // seconds
	Hint     string  `json:"hint,omitempty"`
	Cached   bool    `json:"cached,omitempty"` // fully encoded; replays instantly
}

// player owns the disc session: it detects the disc once, lists its titles
// and runs one streaming pipeline at a time, restarting it on selection.
type player struct {
	o       options
	t       tools
	workDir string
	hlsRoot string // one sub-directory of HLS segments per title
	st      *status
	root    context.Context // cancelled on shutdown

	src      source
	disc     discInfo
	mkv      []string // MakeMKV command when the disc goes through MakeMKV
	mkvTitle map[int]mkvTitle
	cdTracks []cdTrack
	titles   []title

	mu      sync.Mutex
	ready   bool
	gen     int
	current int
	cancel  context.CancelFunc
	done    chan struct{}

	cacheMu  sync.Mutex
	liveDir  string       // directory of the title being served
	complete map[int]bool // titles whose cache is complete
	finished chan error   // receives the result of each pipeline that ends on its own
}

func newPlayer(ctx context.Context, o options, t tools, workDir, hlsRoot string, st *status) *player {
	return &player{o: o, t: t, workDir: workDir, hlsRoot: hlsRoot, st: st, root: ctx,
		finished: make(chan error, 1), complete: map[int]bool{}}
}

// titleDir is where a title's segments are cached.
func (p *player) titleDir(id int) string {
	return filepath.Join(p.hlsRoot, fmt.Sprintf("t%d", id))
}

// servingDir is the directory /live/ currently serves ("" before playback).
func (p *player) servingDir() string {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	return p.liveDir
}

// prepare waits for a disc, detects it and builds the title list.
func (p *player) prepare() error {
	ctx := p.root
	src, err := openSource(ctx, p.o.device, p.o.wait, p.st)
	if err != nil {
		return err
	}
	p.src = src

	p.st.set("detecting", "")
	log.Printf("detecting disc in %s ...", p.o.device)
	d, err := detect(ctx, p.t, src)
	if err != nil {
		return err
	}
	log.Printf("disc: %s, encryption: %s", d.kind, d.encryption)
	p.disc = d
	p.st.setDisc(d)

	useMKV := false
	switch {
	case d.kind == kindAudioCD:
		useMKV = false // audio CDs are never encrypted and MakeMKV can't read them
	case p.o.makemkvMode == "always":
		useMKV = true
	case d.encryption == encYes:
		if p.o.makemkvMode == "never" {
			return errors.New("disc is encrypted but -makemkv=never was given")
		}
		useMKV = true
	case d.encryption == encUnknown || d.kind == kindUnknown:
		if p.o.makemkvMode != "never" {
			useMKV = true
			log.Print("could not determine encryption; using MakeMKV to be safe")
		}
	}
	if d.kind == kindUnknown && !useMKV {
		return errors.New("unrecognised disc (not a DVD-Video, Blu-ray or audio CD)")
	}

	p.st.set("detecting", "reading titles")
	switch {
	case useMKV:
		if p.mkv, err = findMakeMKV(p.workDir); err != nil {
			return err
		}
		log.Print("reading disc structure with MakeMKV (this can take a minute) ...")
		list, err := mkvInfo(ctx, p.mkv, src, p.o.minLength)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return fmt.Errorf("MakeMKV found no titles longer than %ds (try -minlength)", p.o.minLength)
		}
		p.mkvTitle = map[int]mkvTitle{}
		for _, mt := range list {
			id := mt.id + 1
			p.mkvTitle[id] = mt
			p.titles = append(p.titles, title{ID: id, Name: fmt.Sprintf("Title %d", id), Duration: mt.duration.Seconds()})
		}
	case d.kind == kindAudioCD:
		if p.cdTracks, err = readTOC(src.path); err != nil {
			return err
		}
		if len(p.cdTracks) == 0 {
			return errors.New("no audio tracks on this CD")
		}
		for _, tr := range p.cdTracks {
			p.titles = append(p.titles, title{ID: tr.number, Name: fmt.Sprintf("Track %d", tr.number),
				Duration: float64(tr.end-tr.start) / 75})
		}
	case d.kind == kindDVD:
		if p.titles, err = listDVDTitles(ctx, p.t, src.path); err != nil {
			return err
		}
	case d.kind == kindBluRay:
		if p.titles, err = listBlurayPlaylists(ctx, p.t, src.path); err != nil {
			return err
		}
	}
	markPlayAll(p.titles)

	log.Print("titles:")
	for _, t := range p.titles {
		log.Printf("  %-16s %s  %s", t.Name, fmtDur(time.Duration(t.Duration*float64(time.Second))), t.Hint)
	}
	p.st.setTitles(p.titles)

	p.mu.Lock()
	p.ready = true
	p.mu.Unlock()
	return nil
}

// markPlayAll labels titles that are just the other titles played back to
// back (typical on TV series discs), and the longest title otherwise.
func markPlayAll(titles []title) {
	longest := -1
	for i, t := range titles {
		if longest < 0 || t.Duration > titles[longest].Duration {
			longest = i
		}
		var sum float64
		parts := 0
		for _, o := range titles {
			if o.Duration < t.Duration && o.Duration >= t.Duration*0.05 {
				sum += o.Duration
				parts++
			}
		}
		if parts >= 2 && math.Abs(sum-t.Duration) <= t.Duration*0.03 {
			titles[i].Hint = "Play all"
		}
	}
	if longest >= 0 && titles[longest].Hint == "" && len(titles) > 1 {
		titles[longest].Hint = "Longest"
	}
}

// defaultTitle is -title if given, otherwise the longest title.
func (p *player) defaultTitle() int {
	if p.o.title > 0 {
		return p.o.title
	}
	sorted := append([]title(nil), p.titles...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Duration > sorted[j].Duration })
	return sorted[0].ID
}

// play stops whatever is playing and starts the given title.
func (p *player) play(id int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready {
		return errors.New("the disc is not ready yet")
	}
	var sel *title
	for i := range p.titles {
		if p.titles[i].ID == id {
			sel = &p.titles[i]
		}
	}
	if sel == nil {
		return fmt.Errorf("no title %d on this disc", id)
	}

	if p.cancel != nil {
		p.st.set("switching", sel.Name)
		p.cancel()
		<-p.done
		p.cancel = nil
		// A partly cached title can't be resumed (the disc is read from the
		// start of the title), so free its disk space.
		if !p.isComplete(p.current) {
			os.RemoveAll(p.titleDir(p.current))
		}
	}
	p.gen++
	gen := p.gen
	p.current = sel.ID
	dir := p.titleDir(sel.ID)
	p.st.setPlaying(sel.ID, gen)

	if p.isComplete(sel.ID) {
		p.setServing(dir)
		log.Printf("playing %s from cache", sel.Name)
		p.st.set("cached", sel.Name)
		return nil
	}

	// Each run writes uniquely named segments (by generation), so clients
	// never see stale segments from an earlier run.
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p.setServing(dir)
	ctx, cancel := context.WithCancel(p.root)
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	p.st.set("streaming", sel.Name)
	log.Printf("playing %s (%s)", sel.Name, fmtDur(time.Duration(sel.Duration*float64(time.Second))))

	go func() {
		defer close(done)
		start := time.Now()
		err := p.stream(ctx, sel.ID, dir, gen)
		if ctx.Err() != nil {
			return // switched to another title, or shutting down
		}
		log.Printf("%s ran for %s", sel.Name, time.Since(start).Round(time.Second))
		if err != nil {
			log.Printf("error: %v", err)
			p.st.set("error", err.Error())
		} else {
			p.cacheMu.Lock()
			p.complete[sel.ID] = true
			p.cacheMu.Unlock()
			p.st.markCached(sel.ID)
			log.Printf("%s is fully cached", sel.Name)
			p.st.set("cached", sel.Name)
		}
		select {
		case p.finished <- err:
		default:
		}
	}()
	return nil
}

func (p *player) isComplete(id int) bool {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	return p.complete[id]
}

func (p *player) setServing(dir string) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.liveDir = dir
}

func (p *player) stream(ctx context.Context, id int, dir string, gen int) error {
	switch {
	case p.mkv != nil:
		return streamMakeMKV(ctx, p.o, p.t, p.mkv, p.src, p.workDir, dir, p.mkvTitle[id], gen)
	case p.disc.kind == kindAudioCD:
		tracks := p.cdTracks
		for len(tracks) > 0 && tracks[0].number < id {
			tracks = tracks[1:]
		}
		return streamAudioCD(ctx, p.o, p.t, p.src, dir, tracks, gen)
	default:
		return streamDirect(ctx, p.o, p.t, p.src, p.disc, dir, id, gen)
	}
}

// stop ends the current pipeline and waits for it to exit.
func (p *player) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
}
