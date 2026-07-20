package cni

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	netnsBasePath = "/var/run/netns"
	// netnsPrefix scopes GC to cocoon-owned netns (so docker/containerd entries survive).
	netnsPrefix = "cocoon-"

	tapPrefix = "tap"
)

type Config struct {
	*config.Config
}

// NewConfig wraps the shared config with the CNI path layout.
func NewConfig(conf *config.Config) *Config { return &Config{Config: conf} }

func (c *Config) EnsureDirs() error {
	return utils.EnsureDirs(c.dbDir())
}

func (c *Config) IndexFile() string { return filepath.Join(c.dbDir(), "networks.json") }
func (c *Config) IndexLock() string { return filepath.Join(c.dbDir(), "networks.lock") }
func (c *Config) CacheDir() string  { return filepath.Join(c.dir(), "cache") }
func (c *Config) dir() string       { return filepath.Join(c.RootDir, "cni") }
func (c *Config) dbDir() string     { return filepath.Join(c.dir(), "db") }

func netnsPath(vmID string) string {
	return filepath.Join(netnsBasePath, netnsName(vmID))
}

func netnsName(vmID string) string {
	return netnsPrefix + vmID
}
