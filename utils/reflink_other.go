//go:build !linux

package utils

import (
	"context"
	"errors"
)

// ReflinkCopy copies a single file. On non-Linux, falls back to SparseCopy.
func ReflinkCopy(_ context.Context, dst, src string, sync SyncMode) error {
	return SparseCopy(dst, src, sync)
}

// CheckReflink needs FICLONE, so it fails off Linux.
func CheckReflink(string) error {
	return errors.ErrUnsupported
}
