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

// tarFileMaybeSparse writes a file to tw using our custom COCOON.sparse PAX
// format when the file has holes (detected via SEEK_HOLE/SEEK_DATA).
//
// For a 10G COW disk with 25MB of actual data, this writes ~25MB to the tar
// archive instead of 10G — making snapshot creation orders of magnitude faster.
//
// Falls back to a regular tar entry when:
//   - the file is empty or very small
//   - SEEK_HOLE/SEEK_DATA fails (unsupported filesystem)
//   - the file has no holes (not actually sparse)
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
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return fmt.Errorf("seek %s: %w", path, seekErr)
		}
		return tarFileFrom(tw, f, fi, nameInTar)
	}

	var dataSize int64
	for _, seg := range segments {
		dataSize += seg.Length
	}
	if dataSize == size {
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return fmt.Errorf("seek %s: %w", path, seekErr)
		}
		return tarFileFrom(tw, f, fi, nameInTar)
	}

	mapJSON, err := json.Marshal(segments)
	if err != nil {
		return fmt.Errorf("marshal sparse map for %s: %w", path, err)
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
