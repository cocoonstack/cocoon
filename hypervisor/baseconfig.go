package hypervisor

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	defaultSocketWaitTimeout    = 5 * time.Second
	defaultTerminateGracePeriod = 5 * time.Second
)

// BaseConfig holds the directory layout + timeout defaults shared by all hypervisor backends; backends embed it and add binary-specific methods.
type BaseConfig struct {
	*config.Config
	backendName string
}

// NewBaseConfig creates a BaseConfig for the named backend.
func NewBaseConfig(conf *config.Config, name string) BaseConfig {
	return BaseConfig{Config: conf, backendName: name}
}

func (c *BaseConfig) RunDir() string { return filepath.Join(c.Config.RunDir, c.backendName) }

func (c *BaseConfig) LogDir() string { return filepath.Join(c.Config.LogDir, c.backendName) }

func (c *BaseConfig) IndexFile() string { return filepath.Join(c.dbDir(), "vms.json") }

func (c *BaseConfig) IndexLock() string { return filepath.Join(c.dbDir(), "vms.lock") }

func (c *BaseConfig) VMRunDir(vmID string) string { return filepath.Join(c.RunDir(), vmID) }

func (c *BaseConfig) VMLogDir(vmID string) string { return filepath.Join(c.LogDir(), vmID) }

func (c *BaseConfig) COWRawPath(vmID string) string {
	return filepath.Join(c.VMRunDir(vmID), COWRawFileName)
}

// EnsureDirs creates all static directories required by the backend.
func (c *BaseConfig) EnsureDirs() error {
	return utils.EnsureDirs(c.dbDir(), c.RunDir(), c.LogDir())
}

// StopTimeout returns the guest-shutdown grace window; pair with ForceStop, which owns the negative sentinel.
func (c *BaseConfig) StopTimeout() time.Duration {
	return time.Duration(c.StopTimeoutSeconds) * time.Second
}

// ForceStop reports whether --force set the negative StopTimeoutSeconds sentinel (skip graceful shutdown).
func (c *BaseConfig) ForceStop() bool { return c.StopTimeoutSeconds < 0 }

// SocketWaitTimeout returns configured timeout or default (5s).
func (c *BaseConfig) SocketWaitTimeout() time.Duration {
	if c.SocketWaitTimeoutSeconds > 0 {
		return time.Duration(c.SocketWaitTimeoutSeconds) * time.Second
	}
	return defaultSocketWaitTimeout
}

// TerminateGracePeriod returns configured grace period or default (5s).
func (c *BaseConfig) TerminateGracePeriod() time.Duration {
	if c.TerminateGracePeriodSeconds > 0 {
		return time.Duration(c.TerminateGracePeriodSeconds) * time.Second
	}
	return defaultTerminateGracePeriod
}

// LoadAndValidateMeta loads dir's snapshot sidecar and validates its paths against this backend's managed roots.
func (c *BaseConfig) LoadAndValidateMeta(dir string) (*SnapshotMeta, error) {
	return LoadAndValidateMeta(dir, c.RootDir, c.Config.RunDir)
}

// PreflightRestore runs the shared restore preflight against this backend's managed roots.
func (c *BaseConfig) PreflightRestore(srcDir string, rec *VMRecord, integrity IntegrityCheck) (*SnapshotMeta, error) {
	return PreflightRestore(srcDir, c.RootDir, c.Config.RunDir, rec, integrity)
}

func (c *BaseConfig) RootDirPath() string { return c.RootDir }

// ResolveExternalVolume canonicalizes path (EvalSymlinks also asserts existence) and refuses cocoon-managed roots — vm rm / GC delete those trees, and a symlink must not smuggle one past the check.
func (c *BaseConfig) ResolveExternalVolume(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("disk path: %w", err)
	}
	for _, dir := range []string{c.RootDir, c.Config.RunDir, c.Config.LogDir} {
		if r, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
			dir = r
		}
		if IsUnderDir(resolved, dir) {
			return "", fmt.Errorf("external volume path %s is inside a cocoon-managed directory", path)
		}
	}
	return resolved, nil
}

func (c *BaseConfig) dir() string   { return filepath.Join(c.RootDir, c.backendName) }
func (c *BaseConfig) dbDir() string { return filepath.Join(c.dir(), "db") }
