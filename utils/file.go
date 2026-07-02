package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
)

// StaleTempAge is the age threshold for removing stale temp files during GC.
const StaleTempAge = time.Hour

// EnsureDirs creates all directories with 0o750 permissions.
func EnsureDirs(dirs ...string) error {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// FileHead returns up to n bytes from offset 0 without moving the read position. Truncated at EOF, no error if shorter than n.
func FileHead(f *os.File, n int) ([]byte, error) {
	buf := make([]byte, n)
	m, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:m], nil
}

// ValidFile returns true if path is a regular file with size > 0.
func ValidFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// ScanFileStems returns the name-without-suffix of every file in dir whose name ends with suffix.
func ScanFileStems(dir, suffix string) ([]string, error) {
	return scanDir(dir, func(e os.DirEntry) (string, bool) {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return strings.TrimSuffix(e.Name(), suffix), true
		}
		return "", false
	})
}

// ScanSubdirs returns the names of all immediate subdirectories of dir.
func ScanSubdirs(dir string) ([]string, error) {
	return scanDir(dir, func(e os.DirEntry) (string, bool) {
		if e.IsDir() {
			return e.Name(), true
		}
		return "", false
	})
}

// DirSize sums regular file sizes under dir (recursive). Missing dir → 0.
func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

// FilterUnreferenced returns elements of candidates not present in refs or any of the optional exclude sets.
func FilterUnreferenced(candidates []string, refs map[string]struct{}, exclude ...map[string]struct{}) []string {
	var out []string
	for _, s := range candidates {
		if _, ok := refs[s]; ok {
			continue
		}
		excluded := false
		for _, ex := range exclude {
			if _, ok := ex[s]; ok {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, s)
		}
	}
	return out
}

// RemoveMatching removes entries in dir where match returns true, returning per-entry removal errors.
func RemoveMatching(ctx context.Context, dir string, match func(os.DirEntry) bool) []error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return []error{fmt.Errorf("read %s: %w", dir, err)}
	}

	var errs []error
	for _, e := range entries {
		if !match(e) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		} else {
			log.WithFunc("utils.RemoveMatching").Debugf(ctx, "GC removed: %s", path)
		}
	}
	return errs
}

func scanDir(dir string, fn func(os.DirEntry) (string, bool)) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	var result []string
	for _, e := range entries {
		if name, ok := fn(e); ok {
			result = append(result, name)
		}
	}
	return result, nil
}

// copyWithCleanup opens src and creates dst, runs fn to copy the bytes, and removes dst if fn or the final close fails. fn must not close dst.
func copyWithCleanup(dst, src string, fn func(srcFile, dstFile *os.File) error) (err error) {
	srcFile, err := os.Open(src) //nolint:gosec // caller-controlled internal path
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer srcFile.Close() //nolint:errcheck

	dstFile, err := os.Create(dst) //nolint:gosec // caller-controlled internal path
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer func() {
		closeErr := dstFile.Close()
		if err != nil { // fn failed: drop the partial dst, keep fn's error
			_ = os.Remove(dst)
			return
		}
		err = closeErr // fn succeeded: surface a close failure but keep dst
	}()

	return fn(srcFile, dstFile)
}
