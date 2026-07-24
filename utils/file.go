package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
)

// StaleTempAge is the age threshold for removing stale temp files during GC.
const StaleTempAge = time.Hour

// SameFilesystem reports whether a and b live on the same filesystem (matching st_dev), so a rename or hardlink between them can succeed rather than failing with EXDEV.
func SameFilesystem(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	da, ok := sa.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	db, ok := sb.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return da.Dev == db.Dev, nil
}

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

// FileExists reports bare existence; ValidFile additionally demands a
// non-empty regular file.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ValidFile returns true if path is a regular file with size > 0.
func ValidFile(path string) bool {
	_, err := ValidFileSize(path)
	return err == nil
}

// ValidFileSize returns the size of path when it is a regular non-empty file.
func ValidFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, fmt.Errorf("invalid file: %s", path)
	}
	return info.Size(), nil
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
		if slices.ContainsFunc(exclude, func(ex map[string]struct{}) bool { _, ok := ex[s]; return ok }) {
			continue
		}
		out = append(out, s)
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

	logger := log.WithFunc("utils.RemoveMatching")
	var errs []error
	for _, e := range entries {
		if !match(e) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		} else {
			logger.Infof(ctx, "collected %s", path)
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

// fn must not close dst.
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
