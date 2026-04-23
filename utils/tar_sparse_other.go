//go:build !linux

package utils

import (
	"archive/tar"
	"fmt"
	"os"
)

// tarFileMaybeSparse falls back to regular tarFileFrom on non-Linux (no SEEK_HOLE/SEEK_DATA).
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

	return tarFileFrom(tw, f, fi, nameInTar)
}
