package cni

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	netnsBasePath = "/var/run/netns"
	// netnsPrefix prevents GC from deleting netns created by other tools
	// (docker, containerd, etc.). Only netns matching this prefix are managed.
	netnsPrefix = "cocoon-"
)

// Config holds CNI network provider specific configuration, embedding the global config.
type Config struct {
	*config.Config
}

// EnsureDirs creates all static directories required by the CNI network provider.
func (c *Config) EnsureDirs() error {
	return utils.EnsureDirs(
		c.dbDir(),
	)
}

// IndexFile returns the path to the network index JSON file.
func (c *Config) IndexFile() string { return filepath.Join(c.dbDir(), "networks.json") }

// IndexLock returns the path to the network index lock file.
func (c *Config) IndexLock() string { return filepath.Join(c.dbDir(), "networks.lock") }

// CacheDir returns the CNI result cache directory path.
func (c *Config) CacheDir() string { return filepath.Join(c.dir(), "cache") }

func (c *Config) dir() string   { return filepath.Join(c.RootDir, "cni") }
func (c *Config) dbDir() string { return filepath.Join(c.dir(), "db") }

// netnsPath returns the named netns path for a VM.
func netnsPath(vmID string) string {
	return filepath.Join(netnsBasePath, netnsPrefix+vmID)
}

// netnsName returns the named netns name (without path) for a VM.
func netnsName(vmID string) string {
	return netnsPrefix + vmID
}
