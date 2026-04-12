package utils

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// PAX record keys for Cocoon's sparse tar format.
const (
	paxSparseMap  = "COCOON.sparse.map"
	paxSparseSize = "COCOON.sparse.size"

	// sparseBlockSize is the zero-detection block size during extraction.
	sparseBlockSize = 4096
)

// sparseSegment describes one contiguous data region in a sparse file.
type sparseSegment struct {
	Offset int64 `json:"o"`
	Length int64 `json:"l"`
}

// TarDir writes regular files in dir into tw as flat tar entries.
func TarDir(tw *tar.Writer, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if err := tarFileMaybeSparse(tw, filepath.Join(dir, entry.Name()), entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// ExtractTar extracts flat tar entries into dir.
func ExtractTar(dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." {
			continue
		}

		outPath := filepath.Join(dir, name)

		if mapJSON, ok := hdr.PAXRecords[paxSparseMap]; ok {
			realSize, parseErr := strconv.ParseInt(hdr.PAXRecords[paxSparseSize], 10, 64)
			if parseErr != nil {
				return fmt.Errorf("parse sparse size for %s: %w", name, parseErr)
			}
			if err := extractFileSparse(outPath, tr, hdr.FileInfo().Mode(), realSize, mapJSON); err != nil {
				return fmt.Errorf("extract sparse %s: %w", name, err)
			}
		} else {
			if err := extractFile(outPath, tr, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("extract %s: %w", name, err)
			}
		}
	}
}

// tarFileFrom writes an already-opened file as a regular (non-sparse) tar entry.
func tarFileFrom(tw *tar.Writer, f *os.File, fi os.FileInfo, nameInTar string) error {
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return fmt.Errorf("tar header for %s: %w", f.Name(), err)
	}
	hdr.Name = nameInTar

	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", nameInTar, err)
	}

	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("write data %s: %w", nameInTar, err)
	}
	return nil
}

// extractFileSparse restores a sparse file from its segment map.
func extractFileSparse(path string, r io.Reader, perm os.FileMode, realSize int64, mapJSON string) error {
	var segments []sparseSegment
	if err := json.Unmarshal([]byte(mapJSON), &segments); err != nil {
		return fmt.Errorf("decode sparse map: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	if err := f.Truncate(realSize); err != nil {
		return err
	}

	for _, seg := range segments {
		if _, err := f.Seek(seg.Offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(f, r, seg.Length); err != nil {
			return err
		}
	}

	return f.Sync()
}

var sparseBlockPool = sync.Pool{
	New: func() any {
		b := make([]byte, sparseBlockSize)
		return &b
	},
}

func extractFile(path string, r io.Reader, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	bp := sparseBlockPool.Get().(*[]byte)
	buf := *bp
	defer sparseBlockPool.Put(bp)
	var total int64
	endsWithHole := false

	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			var writeErr error
			endsWithHole, writeErr = writeBlockSparse(f, buf[:n])
			if writeErr != nil {
				return writeErr
			}
			total += int64(n)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	// Seeked holes at EOF do not extend file size, so fix it with Truncate.
	if endsWithHole {
		if err := f.Truncate(total); err != nil {
			return err
		}
	}

	return f.Sync()
}

// writeBlockSparse seeks over all-zero chunks instead of writing them.
func writeBlockSparse(f *os.File, chunk []byte) (hole bool, err error) {
	if isAllZero(chunk) {
		_, err = f.Seek(int64(len(chunk)), io.SeekCurrent)
		return true, err
	}
	_, err = f.Write(chunk)
	return false, err
}

// isAllZero reports whether every byte in b is zero.
func isAllZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.NativeEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
