package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Linux <linux/cdrom.h> ioctls and return values.
const (
	cdromDriveStatus = 0x5326
	cdromDiscStatus  = 0x5327
	cdslCurrent      = 0x7fffffff

	cdsNoInfo        = 0
	cdsNoDisc        = 1
	cdsTrayOpen      = 2
	cdsDriveNotReady = 3
	cdsDiscOK        = 4
	cdsAudio         = 100
	cdsMixed         = 105
)

type sourceKind int

const (
	srcDevice sourceKind = iota
	srcImage             // ISO file
	srcFolder            // directory containing VIDEO_TS or BDMV
)

type source struct {
	path string
	kind sourceKind
}

type discKind string

const (
	kindAudioCD discKind = "audio CD"
	kindDVD     discKind = "DVD-Video"
	kindBluRay  discKind = "Blu-ray"
	kindUnknown discKind = "unknown"
)

type encState string

const (
	encNo      encState = "none"
	encYes     encState = "encrypted"
	encUnknown encState = "unknown"
)

type discInfo struct {
	kind       discKind
	encryption encState
}

func cdromIoctl(path string, req, arg uintptr) (int, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return -1, err
	}
	defer syscall.Close(fd)
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}

// openSource resolves the -device argument and, for a real drive, waits
// until a disc is loaded and readable.
func openSource(ctx context.Context, path string, wait bool, st *status) (source, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return source{}, err
	}
	if fi.IsDir() {
		base := strings.ToUpper(filepath.Base(path))
		if base == "VIDEO_TS" || base == "BDMV" {
			path = filepath.Dir(path)
		}
		return source{path, srcFolder}, nil
	}
	if fi.Mode()&os.ModeDevice == 0 {
		return source{path, srcImage}, nil
	}

	announced := false
	for {
		s, err := cdromIoctl(path, cdromDriveStatus, cdslCurrent)
		if err != nil {
			return source{}, fmt.Errorf("%s: %w (is it an optical drive, and are you in its group?)", path, err)
		}
		switch s {
		case cdsDiscOK:
			return source{path, srcDevice}, nil
		case cdsNoDisc, cdsTrayOpen:
			if !wait {
				return source{}, fmt.Errorf("no disc in %s", path)
			}
			if !announced {
				log.Printf("waiting for a disc in %s ...", path)
				st.set("waiting", "insert a disc")
				announced = true
			}
		}
		select {
		case <-ctx.Done():
			return source{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func detect(ctx context.Context, t tools, src source) (discInfo, error) {
	if src.kind == srcDevice {
		if s, err := cdromIoctl(src.path, cdromDiscStatus, 0); err == nil && (s == cdsAudio || s == cdsMixed) {
			return discInfo{kindAudioCD, encNo}, nil
		}
	}

	if isBD, enc := probeBluray(ctx, t, src.path); isBD {
		return discInfo{kindBluRay, enc}, nil
	}
	if ctx.Err() != nil {
		return discInfo{}, ctx.Err()
	}

	r, err := sampleMPEGPS(ctx, src.path)
	if err != nil {
		return discInfo{}, err
	}
	log.Printf("DVD scan: %d clear packets, %d scrambled packets, %d unreadable regions",
		r.clear, r.scrambled, r.ioErrors)
	switch {
	case r.scrambled > 0:
		return discInfo{kindDVD, encYes}, nil
	case r.clear > 0 && r.ioErrors == 0:
		return discInfo{kindDVD, encNo}, nil
	case r.clear > 0:
		return discInfo{kindDVD, encUnknown}, nil
	case r.ioErrors > 0:
		// An unauthenticated drive refuses to return CSS-scrambled sectors.
		return discInfo{kindDVD, encUnknown}, nil
	}
	return discInfo{kindUnknown, encUnknown}, nil
}

// probeBluray asks libbluray (inside ffprobe) whether the source is a
// Blu-ray and whether it is AACS/BD+ protected. libaacs/libbdplus are
// deliberately disabled so the answer reflects the disc, not the host.
func probeBluray(ctx context.Context, t tools, path string) (bool, encState) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.ffprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", "bluray:"+path)
	cmd.Env = blurayEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, encNo
	}
	if os.Getenv("DISCSTREAM_DEBUG") != "" {
		log.Printf("debug: Blu-ray probe: %v: %s", err, stderr.String())
	}
	msg := strings.ToLower(stderr.String())
	if strings.Contains(msg, "aacs") || strings.Contains(msg, "bd+") || strings.Contains(msg, "bdplus") {
		return true, encYes
	}
	return false, encUnknown
}

func blurayEnv() []string {
	return append(os.Environ(), "LIBAACS_PATH=/nonexistent", "LIBBDPLUS_PATH=/nonexistent")
}

type psScan struct{ clear, scrambled, ioErrors int }

// sampleMPEGPS reads sectors spread across a DVD (or its largest VOB) and
// checks the PES_scrambling_control bits that CSS sets on scrambled packs.
func sampleMPEGPS(ctx context.Context, path string) (psScan, error) {
	var r psScan
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		vob, err := largestVOB(path)
		if err != nil {
			return r, nil
		}
		path = vob
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size <= 0 {
		return r, fmt.Errorf("cannot determine size of %s: %v", path, err)
	}

	const samples = 64
	const sector = 2048
	buf := make([]byte, 16*sector)
	deadline := time.Now().Add(90 * time.Second)
	for i := 0; i < samples; i++ {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}
		off := (size * int64(i) / samples) &^ (sector - 1)
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			r.ioErrors++
			if r.ioErrors >= 4 && r.clear == 0 {
				break
			}
			continue
		}
		for s := 0; s+sector <= n; s += sector {
			switch inspectPack(buf[s : s+sector]) {
			case 1:
				r.clear++
			case 2:
				r.scrambled++
			}
		}
		if r.scrambled > 0 {
			break
		}
	}
	return r, nil
}

// inspectPack returns 0 if the sector is not an audio/video MPEG-2 pack,
// 1 if it is clear and 2 if it is scrambled.
func inspectPack(sec []byte) int {
	if sec[0] != 0 || sec[1] != 0 || sec[2] != 1 || sec[3] != 0xBA || sec[4]&0xC0 != 0x40 {
		return 0
	}
	pos := 14 + int(sec[13]&7)
	for pos+9 <= len(sec) {
		if sec[pos] != 0 || sec[pos+1] != 0 || sec[pos+2] != 1 {
			return 0
		}
		sid := sec[pos+3]
		if (sid >= 0xE0 && sid <= 0xEF) || (sid >= 0xC0 && sid <= 0xDF) || sid == 0xBD {
			flags := sec[pos+6]
			if flags&0xC0 != 0x80 {
				return 0
			}
			if (flags>>4)&3 != 0 {
				return 2
			}
			return 1
		}
		pos += 6 + (int(sec[pos+4])<<8 | int(sec[pos+5]))
	}
	return 0
}

func largestVOB(dir string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "VIDEO_TS", "*.VOB"))
	lower, _ := filepath.Glob(filepath.Join(dir, "video_ts", "*.vob"))
	matches = append(matches, lower...)
	best, bestSize := "", int64(-1)
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Size() > bestSize {
			best, bestSize = m, fi.Size()
		}
	}
	if best == "" {
		return "", errors.New("no VOB files")
	}
	return best, nil
}
