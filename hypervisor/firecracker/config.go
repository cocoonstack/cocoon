package firecracker

import (
	"path/filepath"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	metajson "github.com/cocoonstack/cocoon/meta/json"
)

// Config holds Firecracker specific configuration.
type Config struct {
	hypervisor.BaseConfig
}

// NewConfig creates a Config from a global config.
func NewConfig(conf *config.Config) *Config {
	return &Config{BaseConfig: hypervisor.NewBaseConfig(conf, "firecracker")}
}

func (c *Config) BinaryName() string { return filepath.Base(c.FCBinary) }

func (c *Config) PIDFileName() string { return pidFileName }

func (c *Config) COWRawPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), hypervisor.COWRawFileName)
}

// MetaNamespace declares this backend's namespace on the shared meta store.
func MetaNamespace(conf *config.Config) metajson.Namespace {
	return hypervisor.MetaNamespace(typ, NewConfig(conf))
}
