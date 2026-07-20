package images

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/utils"
)

// BaseConfig is the directory layout shared by all image backends.
type BaseConfig struct {
	RootDir string
	Subdir  string
	BlobExt string
}

func (c *BaseConfig) BackendDir() string { return filepath.Join(c.RootDir, c.Subdir) }
func (c *BaseConfig) DBDir() string      { return filepath.Join(c.BackendDir(), "db") }
func (c *BaseConfig) TempDir() string    { return filepath.Join(c.BackendDir(), "temp") }
func (c *BaseConfig) BlobsDir() string   { return filepath.Join(c.BackendDir(), "blobs") }

// BlobLockPath is the per-digest lock beside the blob (design §5): taken by
// GC and by any flow that materializes that digest; never removed by cleanup.
func (c *BaseConfig) BlobLockPath(hex string) string {
	return filepath.Join(c.BlobsDir(), hex+".lock")
}

func (c *BaseConfig) IndexFile() string { return filepath.Join(c.DBDir(), "images.json") }
func (c *BaseConfig) IndexLock() string { return filepath.Join(c.DBDir(), "images.lock") }

func (c *BaseConfig) BlobPath(hex string) string {
	return filepath.Join(c.BlobsDir(), hex+c.BlobExt)
}

// OwnsBlob reports whether this backend holds hex's blob file.
func (c *BaseConfig) OwnsBlob(hex string) bool {
	_, err := os.Stat(c.BlobPath(hex))
	return err == nil
}

func (c *BaseConfig) EnsureBaseDirs() error {
	if c.RootDir == "" {
		return fmt.Errorf("root dir must not be empty")
	}
	return utils.EnsureDirs(c.DBDir(), c.TempDir(), c.BlobsDir())
}

// FirmwarePath returns the UEFI firmware blob (CLOUDHV.fd) under rootDir/firmware.
func FirmwarePath(rootDir string) string {
	return filepath.Join(rootDir, "firmware", "CLOUDHV.fd")
}
