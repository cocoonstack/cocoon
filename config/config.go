package config

import (
	"fmt"
	"net"
	"runtime"
	"strings"

	coretypes "github.com/projecteru2/core/types"
)

// Config holds global Cocoon configuration.
type Config struct {
	// RootDir is the base directory for persistent data (images, firmware, VM DB).
	// Env: COCOON_ROOT_DIR. Default: /var/lib/cocoon.
	RootDir string `json:"root_dir" mapstructure:"root_dir"`
	// RunDir is the base directory for runtime state (PID files, Unix sockets).
	// Contents are ephemeral and may not survive reboots.
	// Env: COCOON_RUN_DIR. Default: /var/lib/cocoon/run.
	RunDir string `json:"run_dir" mapstructure:"run_dir"`
	// LogDir is the base directory for VM and process logs.
	// Env: COCOON_LOG_DIR. Default: /var/log/cocoon.
	LogDir string `json:"log_dir" mapstructure:"log_dir"`
	// CHBinary is the path or name of the cloud-hypervisor executable.
	// Default: "cloud-hypervisor".
	CHBinary string `json:"ch_binary" mapstructure:"ch_binary"`
	// FCBinary is the path or name of the firecracker executable.
	// Default: "firecracker".
	FCBinary string `json:"fc_binary" mapstructure:"fc_binary"`
	// UseFirecracker selects Firecracker as the hypervisor backend.
	// Set via --fc flag. Default: false (use Cloud Hypervisor).
	UseFirecracker bool `json:"use_firecracker,omitempty" mapstructure:"use_firecracker"`
	// StopTimeoutSeconds is how long to wait for a guest to respond to an
	// ACPI power-button before falling back to SIGTERM/SIGKILL.
	// Default: 30.
	StopTimeoutSeconds int `json:"stop_timeout_seconds" mapstructure:"stop_timeout_seconds"`
	// PoolSize is the goroutine pool size for concurrent operations.
	// Defaults to runtime.NumCPU() if zero.
	PoolSize int `json:"pool_size" mapstructure:"pool_size"`
	// CNIConfDir is the directory for CNI plugin configuration files.
	// Default: /etc/cni/net.d.
	CNIConfDir string `json:"cni_conf_dir" mapstructure:"cni_conf_dir"`
	// CNIBinDir is the directory for CNI plugin binaries.
	// Default: /opt/cni/bin.
	CNIBinDir string `json:"cni_bin_dir" mapstructure:"cni_bin_dir"`
	// DefaultRootPassword is the root password injected into cloudimg VMs
	// via cloud-init metadata. Empty means no password is set.
	DefaultRootPassword string `json:"default_root_password" mapstructure:"default_root_password"`
	// DNS is a comma or semicolon separated list of DNS server addresses
	// injected into VM network configuration.
	// Env: COCOON_DNS. Default: "8.8.8.8,1.1.1.1".
	DNS string `json:"dns" mapstructure:"dns"`
	// SocketWaitTimeoutSeconds is how long to wait for the CH API socket
	// after process start. Default: 5. Increase for slow storage.
	SocketWaitTimeoutSeconds int `json:"socket_wait_timeout_seconds,omitempty" mapstructure:"socket_wait_timeout_seconds"`
	// TerminateGracePeriodSeconds is the SIGTERM→SIGKILL window when
	// force-killing a CH process. Default: 5.
	TerminateGracePeriodSeconds int `json:"terminate_grace_period_seconds,omitempty" mapstructure:"terminate_grace_period_seconds"`
	// Log configuration, uses eru core's ServerLogConfig.
	Log *coretypes.ServerLogConfig `json:"log" mapstructure:"log"`
}

// EffectivePoolSize returns PoolSize if set, otherwise runtime.NumCPU().
func (c *Config) EffectivePoolSize() int {
	if c.PoolSize <= 0 {
		return runtime.NumCPU()
	}
	return c.PoolSize
}

// Validate checks that all config fields are within acceptable ranges.
// Should be called once at startup after unmarshalling.
func (c *Config) Validate() error {
	if c.RootDir == "" {
		return fmt.Errorf("root_dir must not be empty")
	}
	if c.RunDir == "" {
		return fmt.Errorf("run_dir must not be empty")
	}
	if c.LogDir == "" {
		return fmt.Errorf("log_dir must not be empty")
	}
	if c.StopTimeoutSeconds <= 0 {
		return fmt.Errorf("stop_timeout_seconds must be > 0, got %d", c.StopTimeoutSeconds)
	}
	if _, err := c.DNSServers(); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	return nil
}

// DNSServers parses the DNS string into a slice of server addresses.
// Returns an error if any entry is not a valid IP address.
func (c *Config) DNSServers() ([]string, error) {
	if c.DNS == "" {
		return nil, nil
	}
	raw := strings.ReplaceAll(c.DNS, ";", ",")
	var servers []string
	for s := range strings.SplitSeq(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if net.ParseIP(s) == nil {
			return nil, fmt.Errorf("invalid DNS server address %q", s)
		}
		servers = append(servers, s)
	}
	return servers, nil
}
