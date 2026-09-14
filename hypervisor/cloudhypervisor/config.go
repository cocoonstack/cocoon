package cloudhypervisor

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
)

type Config struct {
	hypervisor.BaseConfig
}

func NewConfig(conf *config.Config) *Config {
	return &Config{BaseConfig: hypervisor.NewBaseConfig(conf, "cloudhypervisor", conf.CHBinary, pidFileName)}
}

func (c *Config) OverlayPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), "overlay.qcow2")
}

func (c *Config) CidataPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), cidataFile)
}
