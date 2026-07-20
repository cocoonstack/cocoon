package cloudhypervisor

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	metajson "github.com/cocoonstack/cocoon/meta/json"
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

func (c *Config) PIDFileName() string { return pidFileName }

func (c *Config) COWRawPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), hypervisor.COWRawFileName)
}

func (c *Config) OverlayPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), "overlay.qcow2")
}

func (c *Config) CidataPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), cidataFile)
}

// MetaNamespace declares this backend's namespace on the shared meta store.
func MetaNamespace(conf *config.Config) metajson.Namespace {
	cfg := NewConfig(conf)
	return hypervisor.MetaNamespace(typ, cfg.IndexFile(), cfg.IndexLock())
}
