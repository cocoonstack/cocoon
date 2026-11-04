package utils

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const qcow2MaxBackingName = 1023

// Qcow2Header carries the qcow2 header fields cocoon consumes.
type Qcow2Header struct {
	Version        uint32
	VirtualSize    int64
	HasBackingFile bool
	BackingFile    string
}

// ReadQcow2Header parses the qcow2 header and backing file name; ok is false when path is not qcow2.
func ReadQcow2Header(path string) (Qcow2Header, bool, error) {
	f, err := os.Open(path) //nolint:gosec // cocoon-managed or caller-validated path
	if err != nil {
		return Qcow2Header{}, false, err
	}
	defer f.Close() //nolint:errcheck

	var hdr [32]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return Qcow2Header{}, false, nil
	}
	if !bytes.Equal(hdr[:4], Qcow2Magic) {
		return Qcow2Header{}, false, nil
	}
	backingOffset := binary.BigEndian.Uint64(hdr[8:16])
	header := Qcow2Header{
		Version:        binary.BigEndian.Uint32(hdr[4:8]),
		VirtualSize:    int64(binary.BigEndian.Uint64(hdr[24:32])), //nolint:gosec // qcow2 virtual size fits int64
		HasBackingFile: backingOffset != 0,
	}
	if size := binary.BigEndian.Uint32(hdr[16:20]); backingOffset != 0 && size > 0 && size <= qcow2MaxBackingName {
		name := make([]byte, size)
		if _, err := f.ReadAt(name, int64(backingOffset)); err != nil { //nolint:gosec // a bad offset fails the read
			return Qcow2Header{}, false, fmt.Errorf("read backing file name: %w", err)
		}
		header.BackingFile = string(name)
	}
	return header, true, nil
}
