package firecracker

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

// Config holds Firecracker specific configuration, embedding the global config.
type Config struct {
	*config.Config
}

// EnsureDirs creates all static directories required by the Firecracker backend.
func (c *Config) EnsureDirs() error {
	return utils.EnsureDirs(
		c.dbDir(),
		c.RunDir(),
		c.LogDir(),
	)
}

// RunDir returns the top-level FC runtime directory.
func (c *Config) RunDir() string { return filepath.Join(c.Config.RunDir, "firecracker") }

// LogDir returns the top-level FC log directory.
func (c *Config) LogDir() string { return filepath.Join(c.Config.LogDir, "firecracker") }

// IndexFile returns the VM index store path.
func (c *Config) IndexFile() string { return filepath.Join(c.dbDir(), "vms.json") }

// IndexLock returns the VM index lock path.
func (c *Config) IndexLock() string { return filepath.Join(c.dbDir(), "vms.lock") }

// VMRunDir returns the per-VM runtime directory.
func (c *Config) VMRunDir(vmID string) string { return filepath.Join(c.RunDir(), vmID) }

// VMLogDir returns the per-VM log directory.
func (c *Config) VMLogDir(vmID string) string { return filepath.Join(c.LogDir(), vmID) }

// COWRawPath returns the path for the OCI COW raw disk.
func (c *Config) COWRawPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), cowFileName)
}

// SocketWaitTimeout returns the configured socket wait timeout or the default.
func (c *Config) SocketWaitTimeout() time.Duration {
	if c.SocketWaitTimeoutSeconds > 0 {
		return time.Duration(c.SocketWaitTimeoutSeconds) * time.Second
	}
	return defaultSocketWaitTimeout
}

// TerminateGracePeriod returns the configured SIGTERM→SIGKILL grace period or the default.
func (c *Config) TerminateGracePeriod() time.Duration {
	if c.TerminateGracePeriodSeconds > 0 {
		return time.Duration(c.TerminateGracePeriodSeconds) * time.Second
	}
	return defaultTerminateGracePeriod
}

// BinaryName returns the base name of the Firecracker binary.
func (c *Config) BinaryName() string { return filepath.Base(c.FCBinary) }

// PIDFileName returns the PID file name for the Firecracker backend.
func (c *Config) PIDFileName() string { return pidFileName }

func (c *Config) dir() string   { return filepath.Join(c.RootDir, "firecracker") }
func (c *Config) dbDir() string { return filepath.Join(c.dir(), "db") }
