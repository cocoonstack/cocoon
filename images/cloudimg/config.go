package cloudimg

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/images"
)

type Config struct {
	images.BaseConfig
}

func NewConfig(rootDir string) *Config {
	return &Config{BaseConfig: images.BaseConfig{
		RootDir: rootDir, Subdir: "cloudimg", BlobExt: ".qcow2",
	}}
}

func (c *Config) EnsureDirs() error {
	return c.EnsureBaseDirs()
}

// tmpBlobPath uses a hidden prefix so a partial write is safe under last-writer-wins.
func (c *Config) tmpBlobPath(digestHex string) string {
	return filepath.Join(c.TempDir(), ".tmp-"+digestHex+".qcow2")
}
