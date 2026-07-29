package cloudimg

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// NamespaceName is this backend's meta namespace.
	NamespaceName    = "images_cloudimg"
	defaultPullConns = 8
)

type Config struct {
	images.BaseConfig
	// PullConns bounds concurrent HTTP Range connections per cloud-image download.
	PullConns int
}

func NewConfig(rootDir string, pullConns int) *Config {
	return &Config{
		BaseConfig: images.BaseConfig{RootDir: rootDir, Subdir: "cloudimg", BlobExt: ".qcow2", Name: NamespaceName},
		PullConns:  utils.OrDefault(pullConns, defaultPullConns),
	}
}

// tmpBlobPath uses a hidden prefix so a partial write is safe under last-writer-wins.
func (c *Config) tmpBlobPath(digestHex string) string {
	return filepath.Join(c.TempDir(), ".tmp-"+digestHex+".qcow2")
}
