//go:build linux

package utils

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

// Sparse-map JSON cap (margin under tar's 1MB PAX block); var so tests can override.
var maxSparseMapJSONSize = 800 * 1024

// tarFileMaybeSparse writes file as COCOON.sparse PAX when it has holes; falls back to a regular entry on empty files, unsupported FS, no holes, or an oversized segment map.
func tarFileMaybeSparse(tw *tar.Writer, path, nameInTar string) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	size := fi.Size()

	if size == 0 {
		return tarFileFrom(tw, f, fi, nameInTar)
	}

	segments, err := scanDataSegments(int(f.Fd()), size)
	if err != nil {
		// SEEK_HOLE/SEEK_DATA unsupported (e.g. tmpfs, NFS). Fall back.
		return rewindAndTarFull(tw, f, fi, path, nameInTar)
	}

	var dataSize int64
	for _, seg := range segments {
		dataSize += seg.Length
	}
	if dataSize == size {
		return rewindAndTarFull(tw, f, fi, path, nameInTar)
	}

	mapJSON, err := json.Marshal(segments)
	if err != nil {
		return fmt.Errorf("marshal sparse map for %s: %w", path, err)
	}

	// Segment-map JSON exceeds tar's 1MB PAX cap. Fall back to non-sparse.
	if len(mapJSON) > maxSparseMapJSONSize {
		return rewindAndTarFull(tw, f, fi, path, nameInTar)
	}

	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return fmt.Errorf("tar header for %s: %w", path, err)
	}
	hdr.Name = nameInTar
	hdr.Size = dataSize // Only actual data bytes in the tar entry.
	hdr.PAXRecords = map[string]string{
		paxSparseMap:  string(mapJSON),
		paxSparseSize: strconv.FormatInt(size, 10),
	}

	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", nameInTar, err)
	}

	for _, seg := range segments {
		if _, seekErr := f.Seek(seg.Offset, io.SeekStart); seekErr != nil {
			return fmt.Errorf("seek %s to %d: %w", path, seg.Offset, seekErr)
		}
		if _, copyErr := io.CopyN(tw, f, seg.Length); copyErr != nil {
			return fmt.Errorf("copy segment at %d len %d from %s: %w", seg.Offset, seg.Length, path, copyErr)
		}
	}

	return nil
}

func rewindAndTarFull(tw *tar.Writer, f *os.File, fi os.FileInfo, path, nameInTar string) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}
	return tarFileFrom(tw, f, fi, nameInTar)
}
