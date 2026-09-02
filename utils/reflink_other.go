//go:build !linux

package utils

import "context"

// ReflinkCopy copies a single file. On non-Linux, falls back to SparseCopy.
func ReflinkCopy(_ context.Context, dst, src string, sync SyncMode) error {
	return SparseCopy(dst, src, sync)
}
