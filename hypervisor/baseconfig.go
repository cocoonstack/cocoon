package hypervisor

import (
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	defaultSocketWaitTimeout    = 5 * time.Second
	defaultTerminateGracePeriod = 5 * time.Second
)

// BaseConfig holds the directory layout and timeout defaults shared by
// all hypervisor backends. Each backend embeds BaseConfig and adds
// backend-specific methods (BinaryName, PIDFileName, etc.).
type BaseConfig struct {
	*config.Config
	backendName string
}

// NewBaseConfig creates a BaseConfig for the named backend.
func NewBaseConfig(conf *config.Config, name string) BaseConfig {
	return BaseConfig{Config: conf, backendName: name}
}

// RunDir returns the top-level runtime directory for this backend.
func (c *BaseConfig) RunDir() string { return filepath.Join(c.Config.RunDir, c.backendName) }

// LogDir returns the top-level log directory for this backend.
func (c *BaseConfig) LogDir() string { return filepath.Join(c.Config.LogDir, c.backendName) }

// IndexFile returns the VM index store path.
func (c *BaseConfig) IndexFile() string { return filepath.Join(c.dbDir(), "vms.json") }

// IndexLock returns the VM index lock path.
func (c *BaseConfig) IndexLock() string { return filepath.Join(c.dbDir(), "vms.lock") }

// VMRunDir returns the per-VM runtime directory.
func (c *BaseConfig) VMRunDir(vmID string) string { return filepath.Join(c.RunDir(), vmID) }

// VMLogDir returns the per-VM log directory.
func (c *BaseConfig) VMLogDir(vmID string) string { return filepath.Join(c.LogDir(), vmID) }

// EnsureDirs creates all static directories required by the backend.
func (c *BaseConfig) EnsureDirs() error {
	return utils.EnsureDirs(c.dbDir(), c.RunDir(), c.LogDir())
}

// SocketWaitTimeout returns the configured socket wait timeout or the default (5s).
func (c *BaseConfig) SocketWaitTimeout() time.Duration {
	if c.SocketWaitTimeoutSeconds > 0 {
		return time.Duration(c.SocketWaitTimeoutSeconds) * time.Second
	}
	return defaultSocketWaitTimeout
}

// TerminateGracePeriod returns the configured SIGTERM→SIGKILL grace period or the default (5s).
func (c *BaseConfig) TerminateGracePeriod() time.Duration {
	if c.TerminateGracePeriodSeconds > 0 {
		return time.Duration(c.TerminateGracePeriodSeconds) * time.Second
	}
	return defaultTerminateGracePeriod
}

func (c *BaseConfig) dir() string   { return filepath.Join(c.RootDir, c.backendName) }
func (c *BaseConfig) dbDir() string { return filepath.Join(c.dir(), "db") }
