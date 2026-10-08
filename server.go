package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

//go:embed player.html
var playerHTML []byte

type status struct {
	mu         sync.Mutex
	State      string  `json:"state"`
	Detail     string  `json:"detail,omitempty"`
	Disc       string  `json:"disc,omitempty"`
	Encryption string  `json:"encryption,omitempty"`
	Ready      bool    `json:"ready"`
	Titles     []title `json:"titles"`
	Current    int     `json:"current"`
	Gen        int     `json:"gen"`
	Cached     float64 `json:"cached"`   // seconds of the current title encoded so far
	Complete   bool    `json:"complete"` // the current title is fully cached
}

func (s *status) set(state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State, s.Detail = state, detail
}

func (s *status) setDisc(d discInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Disc, s.Encryption = string(d.kind), string(d.encryption)
}

func (s *status) setTitles(t []title) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Titles = t
}

func (s *status) setPlaying(id, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Current, s.Gen = id, gen
}

func (s *status) markCached(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Titles {
		if s.Titles[i].ID == id {
			s.Titles[i].Cached = true
		}
	}
}

// refresh updates the playback fields from the current playlist on disk.
func (s *status) refresh(p *player) {
	var m3u8 []byte
	if dir := p.servingDir(); dir != "" {
		m3u8, _ = os.ReadFile(filepath.Join(dir, "index.m3u8"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Ready = m3u8 != nil
	s.Cached, s.Complete = playlistProgress(m3u8)
}

// playlistProgress returns the seconds listed in an HLS playlist and whether
// it is finished.
func playlistProgress(m3u8 []byte) (float64, bool) {
	var total float64
	for _, line := range strings.Split(string(m3u8), "\n") {
		if d, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			d, _, _ = strings.Cut(d, ",")
			v, _ := strconv.ParseFloat(d, 64)
			total += v
		}
	}
	return total, strings.Contains(string(m3u8), "#EXT-X-ENDLIST")
}

// startServer listens on o.port. If the port is taken and wasn't chosen
// explicitly, the next free port is used; the port actually used is returned.
func startServer(o options, portExplicit bool, st *status, p *player) (*http.Server, int, error) {
	mime.AddExtensionType(".m3u8", "application/vnd.apple.mpegurl")
	mime.AddExtensionType(".ts", "video/mp2t")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(playerHTML)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st.refresh(p)
		st.mu.Lock()
		b, _ := json.Marshal(st)
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("POST /play", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.FormValue("id"))
		if err == nil {
			err = p.play(id)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /live/{file}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		dir := p.servingDir()
		name := r.PathValue("file")
		if dir == "" || name != filepath.Base(name) {
			http.NotFound(w, r)
			return
		}
		if name != "index.m3u8" {
			http.ServeFile(w, r, filepath.Join(dir, name))
			return
		}
		// The playlist grows while the title is cached ahead, which players
		// treat as live and would join at the newest segment. EXT-X-START
		// makes them start at the beginning instead.
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		b = bytes.Replace(b, []byte("#EXTM3U\n"), []byte("#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n"), 1)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})

	port := o.port
	ln, err := net.Listen("tcp", net.JoinHostPort(o.bind, strconv.Itoa(port)))
	for tries := 0; err != nil && errors.Is(err, syscall.EADDRINUSE) && !portExplicit && tries < 20; tries++ {
		port++
		ln, err = net.Listen("tcp", net.JoinHostPort(o.bind, strconv.Itoa(port)))
	}
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			err = fmt.Errorf("port %d is already in use by another program; choose another with -port", o.port)
		}
		return nil, 0, err
	}
	if port != o.port {
		log.Printf("port %d is in use, using %d instead", o.port, port)
	}
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	return srv, port, nil
}

// playerURLs returns the web player's address on each LAN interface.
func playerURLs(o options) []string {
	hosts := []string{o.bind}
	if o.bind == "0.0.0.0" || o.bind == "" || o.bind == "::" {
		hosts = nil
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
		if len(hosts) == 0 {
			hosts = []string{"localhost"}
		}
	}
	var urls []string
	for _, h := range hosts {
		urls = append(urls, fmt.Sprintf("http://%s:%d/", h, o.port))
	}
	return urls
}

func printURLs(o options) {
	log.Print("streaming on:")
	for _, u := range playerURLs(o) {
		log.Printf("  browser:        %s", u)
		log.Printf("  VLC/mpv/Kodi:   %slive/index.m3u8", u)
	}
}
