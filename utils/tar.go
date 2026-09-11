package utils

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

const (
	paxSparseMap  = "COCOON.sparse.map"
	paxSparseSize = "COCOON.sparse.size"

	// sparseBlockSize is the zero-detection block size during extraction.
	sparseBlockSize = 4096
	// extractReadBuf bounds one read; runs of data or zero blocks inside it coalesce into one write or one seek.
	extractReadBuf = 1 << 20
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

// ExtractTar extracts flat tar entries into dir; entries matching any skip predicate are dropped. It never fsyncs — callers needing durability follow with SyncTree.
func ExtractTar(dir string, r io.Reader, skip ...func(name string) bool) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." || slices.ContainsFunc(skip, func(fn func(string) bool) bool { return fn(name) }) {
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
			if err := extractFile(outPath, tr, hdr.FileInfo().Mode(), hdr.Size); err != nil {
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
func extractFileSparse(path string, r io.Reader, perm os.FileMode, realSize int64, mapJSON string) (err error) {
	var segments []sparseSegment
	if err = json.Unmarshal([]byte(mapJSON), &segments); err != nil {
		return fmt.Errorf("decode sparse map: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

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

	return nil
}

func extractFile(path string, r io.Reader, perm os.FileMode, size int64) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	buf := make([]byte, min(int64(extractReadBuf), max(size, sparseBlockSize)))
	var total int64
	endsWithHole := false

	for {
		n, readErr := io.ReadFull(r, buf)
		for chunk := buf[:n]; len(chunk) > 0; {
			hole := isAllZero(chunk[:min(sparseBlockSize, len(chunk))])
			run := min(sparseBlockSize, len(chunk))
			for run < len(chunk) && isAllZero(chunk[run:min(run+sparseBlockSize, len(chunk))]) == hole {
				run += sparseBlockSize
			}
			run = min(run, len(chunk))
			if hole {
				_, err = f.Seek(int64(run), io.SeekCurrent)
			} else {
				_, err = f.Write(chunk[:run])
			}
			if err != nil {
				return err
			}
			endsWithHole = hole
			chunk = chunk[run:]
		}
		total += int64(n)
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
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

	return nil
}

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
