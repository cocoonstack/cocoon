package utils

import (
	"context"
	"fmt"
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

// ValidFile returns true if path is a regular file with size > 0.
func ValidFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// ScanFileStems returns the name-without-suffix of every file in dir whose
// name ends with suffix. Used by GC to enumerate on-disk blobs.
func ScanFileStems(dir, suffix string) ([]string, error) {
	return scanDir(dir, func(e os.DirEntry) (string, bool) {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return strings.TrimSuffix(e.Name(), suffix), true
		}
		return "", false
	})
}

// ScanSubdirs returns the names of all immediate subdirectories of dir.
// Used by GC to enumerate per-VM runtime and log directories.
func ScanSubdirs(dir string) ([]string, error) {
	return scanDir(dir, func(e os.DirEntry) (string, bool) {
		if e.IsDir() {
			return e.Name(), true
		}
		return "", false
	})
}

// FilterUnreferenced returns the elements of candidates not present in refs
// or any of the optional exclude sets. Used by GC Resolve to compute deletions.
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

// RemoveMatching scans dir and removes entries where match returns true.
// Returns a slice of errors for entries that could not be removed.
func RemoveMatching(ctx context.Context, dir string, match func(os.DirEntry) bool) []error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
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
			log.WithFunc("gc").Debugf(ctx, "GC removed: %s", path)
		}
	}
	return errs
}

func scanDir(dir string, fn func(os.DirEntry) (string, bool)) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
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
