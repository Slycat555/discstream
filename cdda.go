package main

import (
	"context"
	"io"
	"log"
	"syscall"
	"unsafe"
)

// Linux <linux/cdrom.h> structures for reading digital audio.
const (
	cdromReadTOCHdr   = 0x5305
	cdromReadTOCEntry = 0x5306
	cdromReadAudio    = 0x530e
	cdromLBA          = 0x01
	cdromLeadout      = 0xAA
	cdFrameSizeRaw    = 2352
	cddaFramesPerRead = 25
)

type tocHeader struct{ first, last uint8 }

type tocEntry struct {
	track    uint8
	adrCtrl  uint8 // adr in the low nibble, ctrl in the high nibble
	format   uint8
	_        uint8
	lba      int32
	datamode uint8
	_        [3]uint8
}

type readAudio struct {
	lba     int32
	format  uint8
	_       [3]uint8
	nframes int32
	_       [4]uint8
	buf     unsafe.Pointer
}

type cdTrack struct {
	number     int
	start, end int32 // LBA, end exclusive
}

func ioctlPtr(fd int, req uintptr, p unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(p)); e != 0 {
		return e
	}
	return nil
}

// readTOC returns the audio tracks on the disc (data tracks are skipped).
func readTOC(dev string) ([]cdTrack, error) {
	fd, err := syscall.Open(dev, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)

	var hdr tocHeader
	if err := ioctlPtr(fd, cdromReadTOCHdr, unsafe.Pointer(&hdr)); err != nil {
		return nil, err
	}
	type entry struct {
		number int
		lba    int32
		data   bool
	}
	var entries []entry
	for n := int(hdr.first); n <= int(hdr.last)+1; n++ {
		e := tocEntry{track: uint8(n), format: cdromLBA}
		if n == int(hdr.last)+1 {
			e.track = cdromLeadout
		}
		if err := ioctlPtr(fd, cdromReadTOCEntry, unsafe.Pointer(&e)); err != nil {
			return nil, err
		}
		entries = append(entries, entry{n, e.lba, (e.adrCtrl>>4)&0x04 != 0})
	}
	var tracks []cdTrack
	for i := 0; i+1 < len(entries); i++ {
		if !entries[i].data {
			tracks = append(tracks, cdTrack{entries[i].number, entries[i].lba, entries[i+1].lba})
		}
	}
	return tracks, nil
}

// readCDDA writes raw 44.1 kHz 16-bit stereo PCM for the tracks to w.
// Unreadable frames are replaced by silence so the timeline stays intact.
func readCDDA(ctx context.Context, dev string, tracks []cdTrack, w io.Writer) error {
	fd, err := syscall.Open(dev, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	buf := make([]byte, cddaFramesPerRead*cdFrameSizeRaw)
	for _, tr := range tracks {
		failures := 0
		for lba := tr.start; lba < tr.end; {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			n := min(int32(cddaFramesPerRead), tr.end-lba)
			ra := readAudio{lba: lba, format: cdromLBA, nframes: n, buf: unsafe.Pointer(&buf[0])}
			var err error
			for try := 0; try < 3; try++ {
				if err = ioctlPtr(fd, cdromReadAudio, unsafe.Pointer(&ra)); err == nil {
					break
				}
			}
			chunk := buf[:int(n)*cdFrameSizeRaw]
			if err != nil {
				failures++
				// A long unreadable run at the end of a track is the gap
				// before a data session (CD-Extra); skip the rest of it.
				if failures > 30 {
					log.Printf("track %d: giving up on unreadable area at sector %d", tr.number, lba)
					break
				}
				clear(chunk)
			} else {
				failures = 0
			}
			if _, err := w.Write(chunk); err != nil {
				return err
			}
			lba += n
		}
	}
	return nil
}
