//go:build !linux

package utils

import (
	"errors"
	"os"
)

// FileExtents needs FIEMAP, so it fails off Linux.
func FileExtents(*os.File) ([]Extent, error) {
	return nil, errors.ErrUnsupported
}

// PunchHole needs fallocate, so it fails off Linux.
func PunchHole(*os.File, int64, int64) error {
	return errors.ErrUnsupported
}
