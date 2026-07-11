package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// Sync fsyncs the written output; NoSync skips that for transient artifacts rebuilt from a durable source on retry.
	Sync   SyncMode = true
	NoSync SyncMode = false
)

// SyncMode selects whether a write helper fsyncs its output.
type SyncMode bool

// AtomicWriteFile writes data via temp + fsync + rename so readers never see a partial file.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFile(path, data, perm, true)
}

// AtomicWriteFileNoSync is AtomicWriteFile without fsyncs, for transient run-dir files regenerated on the next launch.
func AtomicWriteFileNoSync(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFile(path, data, perm, false)
}

// AtomicWriteJSON marshals v to JSON and writes it atomically.
func AtomicWriteJSON(path string, v any) error {
	return atomicWriteJSON(path, v, true)
}

// AtomicWriteJSONNoSync marshals v and writes it atomically without fsync (see AtomicWriteFileNoSync).
func AtomicWriteJSONNoSync(path string, v any) error {
	return atomicWriteJSON(path, v, false)
}

// ReadJSONFile loads path and unmarshals it into v.
func ReadJSONFile(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// SyncParentDir fsyncs the directory containing the file to ensure the directory entry is persisted.
func SyncParentDir(dir string) error {
	parent, err := os.Open(dir) //nolint:gosec // directory is derived from cocoon-managed target path
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck

	if err := parent.Sync(); err != nil &&
		!errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EBADF) {
		return err
	}
	return nil
}

// SyncTree fsyncs every regular file directly under dir (snapshot dirs are flat), then dir and its parent — the durable-before-kill barrier hibernate needs, since a bare rename fsyncs nothing.
func SyncTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := syncFile(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	if err := SyncParentDir(dir); err != nil {
		return err
	}
	return SyncParentDir(filepath.Dir(dir))
}

func atomicWriteFile(path string, data []byte, perm os.FileMode, sync bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if sync {
		if err = tmp.Sync(); err != nil {
			return fmt.Errorf("sync temp file: %w", err)
		}
	}
	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to target: %w", err)
	}
	if sync {
		if err = SyncParentDir(dir); err != nil {
			return fmt.Errorf("sync parent dir: %w", err)
		}
	}
	return nil
}

func atomicWriteJSON(path string, v any, sync bool) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, 0o644, sync)
}

func syncFile(path string) (err error) {
	f, err := os.Open(path) //nolint:gosec // path is a cocoon-managed snapshot file
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.Sync()
}
