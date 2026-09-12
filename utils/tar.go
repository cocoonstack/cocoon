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

// maxSparseMapJSONSize keeps the sparse map under tar's 1 MiB PAX block; a var so tests can lower it.
var maxSparseMapJSONSize = 800 * 1024

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

	if len(mapJSON) > maxSparseMapJSONSize {
		return rewindAndTarFull(tw, f, fi, path, nameInTar)
	}

	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return fmt.Errorf("tar header for %s: %w", path, err)
	}
	hdr.Name = nameInTar
	hdr.Size = dataSize // only actual data bytes in the tar entry
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
