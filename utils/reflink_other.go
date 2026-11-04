//go:build !linux

package utils

import (
	"context"
	"errors"
)

// ReflinkCopy falls back to SparseCopy.
func ReflinkCopy(_ context.Context, dst, src string, mode SyncMode) error {
	return SparseCopy(dst, src, mode)
}

// CheckReflink needs FICLONE, so it fails off Linux.
func CheckReflink(string) error {
	return errors.ErrUnsupported
}
