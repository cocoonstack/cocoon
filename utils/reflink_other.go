//go:build !linux

package utils

// ReflinkCopy copies a single file. On non-Linux, falls back to SparseCopy.
func ReflinkCopy(dst, src string, sync SyncMode) error {
	return SparseCopy(dst, src, sync)
}
