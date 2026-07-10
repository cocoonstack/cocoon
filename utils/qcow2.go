package utils

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

// Qcow2Header carries the qcow2 header fields cocoon consumes.
type Qcow2Header struct {
	Version        uint32
	VirtualSize    int64
	HasBackingFile bool
}

// ReadQcow2Header parses the leading 32 bytes of path; ok=false when the file is not qcow2 (wrong magic or shorter than the header).
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
	return Qcow2Header{
		Version:        binary.BigEndian.Uint32(hdr[4:8]),
		VirtualSize:    int64(binary.BigEndian.Uint64(hdr[24:32])), //nolint:gosec // qcow2 virtual size fits int64
		HasBackingFile: binary.BigEndian.Uint64(hdr[8:16]) != 0,
	}, true, nil
}
