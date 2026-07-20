package cloudimg

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/images"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/utils"
)

const metaNS = "images_cloudimg"

// defaultPullConns is the default concurrent Range connections per cloud-image download.
const defaultPullConns = 8

type Config struct {
	images.BaseConfig
	// PullConns bounds concurrent HTTP Range connections per cloud-image download.
	PullConns int
}

func NewConfig(rootDir string, pullConns int) *Config {
	return &Config{
		BaseConfig: images.BaseConfig{RootDir: rootDir, Subdir: "cloudimg", BlobExt: ".qcow2"},
		PullConns:  utils.OrDefault(pullConns, defaultPullConns),
	}
}

func (c *Config) EnsureDirs() error {
	return c.EnsureBaseDirs()
}

// tmpBlobPath uses a hidden prefix so a partial write is safe under last-writer-wins.
func (c *Config) tmpBlobPath(digestHex string) string {
	return filepath.Join(c.TempDir(), ".tmp-"+digestHex+".qcow2")
}

// MetaNamespace declares this backend's namespace on the shared meta store.
func MetaNamespace(rootDir string) metajson.Namespace {
	cfg := NewConfig(rootDir, 0)
	return images.MetaNamespace[imageEntry](metaNS, cfg.IndexFile(), cfg.IndexLock())
}
