package cloudhypervisor

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
)

// Config holds Cloud Hypervisor specific configuration.
type Config struct {
	hypervisor.BaseConfig
}

// NewConfig creates a Config from a global config.
func NewConfig(conf *config.Config) *Config {
	return &Config{BaseConfig: hypervisor.NewBaseConfig(conf, "cloudhypervisor")}
}

func (c *Config) BinaryName() string { return filepath.Base(c.CHBinary) }

func (c *Config) PIDFileName() string { return "ch.pid" }

func (c *Config) COWRawPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), "cow.raw")
}

func (c *Config) OverlayPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), "overlay.qcow2")
}

func (c *Config) CidataPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), cidataFile)
}
