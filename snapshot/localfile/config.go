package localfile

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/utils"
)

// Config holds localfile snapshot backend configuration, embedding the global config.
type Config struct {
	*config.Config
}

// NewConfig creates a Config from a global config.
func NewConfig(conf *config.Config) *Config {
	return &Config{Config: conf}
}

// EnsureDirs creates all required directories for the localfile backend.
func (c *Config) EnsureDirs() error {
	return utils.EnsureDirs(
		c.dbDir(),
		c.DataDir(),
	)
}

// DataDir returns the top-level directory for snapshot data.
func (c *Config) DataDir() string { return filepath.Join(c.dir(), "localfile") }

// SnapshotDataDir returns the per-snapshot data directory.
func (c *Config) SnapshotDataDir(id string) string { return filepath.Join(c.DataDir(), id) }

// IndexFile returns the snapshot index store path.
func (c *Config) IndexFile() string { return filepath.Join(c.dbDir(), "snapshots.json") }

// IndexLock returns the snapshot index lock path.
func (c *Config) IndexLock() string { return filepath.Join(c.dbDir(), "snapshots.lock") }

func (c *Config) dir() string   { return filepath.Join(c.RootDir, "snapshot") }
func (c *Config) dbDir() string { return filepath.Join(c.dir(), "db") }
