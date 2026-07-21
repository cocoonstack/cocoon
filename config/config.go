package config

import (
	"fmt"
	"net"
	"strings"

	coretypes "github.com/projecteru2/core/types"

	"github.com/cocoonstack/cocoon/utils"
)

const (
	HypervisorCloudHypervisor HypervisorType = "cloud-hypervisor"
	HypervisorFirecracker     HypervisorType = "firecracker"

	MetaBackendJSON   = "json"
	MetaBackendSQLite = "sqlite"

	// defaultPullConns is the default concurrent Range connections per cloud-image download.
	defaultPullConns = 8
	// maxPullConns caps it so a misconfigured pull_conns can't exhaust file descriptors.
	maxPullConns = 64
)

// HypervisorType identifies the selected hypervisor backend.
type HypervisorType string

// Config holds global Cocoon configuration.
type Config struct {
	// RootDir: persistent data (images, firmware, VM DB). Env: COCOON_ROOT_DIR. Default: /var/lib/cocoon.
	RootDir string `json:"root_dir" mapstructure:"root_dir"`
	// RunDir: ephemeral runtime state (PID files, sockets). Env: COCOON_RUN_DIR. Default: /var/lib/cocoon/run.
	RunDir string `json:"run_dir" mapstructure:"run_dir"`
	// LogDir: VM and process logs. Env: COCOON_LOG_DIR. Default: /var/log/cocoon.
	LogDir string `json:"log_dir" mapstructure:"log_dir"`
	// CHBinary: path or name of the cloud-hypervisor executable. Default: "cloud-hypervisor".
	CHBinary string `json:"ch_binary" mapstructure:"ch_binary"`
	// FCBinary: path or name of the firecracker executable. Default: "firecracker".
	FCBinary string `json:"fc_binary" mapstructure:"fc_binary"`
	// UseFirecracker selects the Firecracker backend (--fc flag). Default: false (Cloud Hypervisor).
	UseFirecracker bool `json:"use_firecracker,omitempty" mapstructure:"use_firecracker"`
	// PinHypervisor restricts multi-backend operations (list/gc) and the legacy
	// json VM namespace to Hypervisor(). It does not narrow whole-store conversion.
	PinHypervisor bool `json:"pin_hypervisor,omitempty" mapstructure:"pin_hypervisor"`
	// StopTimeoutSeconds: guest ACPI grace before SIGTERM/SIGKILL escalation. Default: 30.
	StopTimeoutSeconds int `json:"stop_timeout_seconds" mapstructure:"stop_timeout_seconds"`
	// PoolSize: goroutine pool size for concurrent operations; 0 = runtime.NumCPU().
	PoolSize int `json:"pool_size" mapstructure:"pool_size"`
	// PullConns: concurrent HTTP Range connections per cloud-image download; <=0 = 8.
	PullConns int `json:"pull_conns" mapstructure:"pull_conns"`
	// MetaBackend selects the metadata engine: "json" or "sqlite"; empty
	// auto-resolves (an existing store binds its engine, fresh roots get sqlite).
	MetaBackend string `json:"meta_backend,omitempty" mapstructure:"meta_backend"`
	// CNIConfDir: CNI plugin configuration dir. Default: /etc/cni/net.d.
	CNIConfDir string `json:"cni_conf_dir" mapstructure:"cni_conf_dir"`
	// CNIBinDir: CNI plugin binary dir. Default: /opt/cni/bin.
	CNIBinDir string `json:"cni_bin_dir" mapstructure:"cni_bin_dir"`
	// DNS: comma/semicolon-separated DNS servers injected into VM net config. Env: COCOON_DNS. Default: "8.8.8.8,1.1.1.1".
	DNS string `json:"dns" mapstructure:"dns"`
	// SocketWaitTimeoutSeconds: wait for the CH API socket after start. Default: 5; increase for slow storage.
	SocketWaitTimeoutSeconds int `json:"socket_wait_timeout_seconds" mapstructure:"socket_wait_timeout_seconds"`
	// TerminateGracePeriodSeconds: SIGTERM→SIGKILL window when force-killing CH. Default: 5.
	TerminateGracePeriodSeconds int                        `json:"terminate_grace_period_seconds" mapstructure:"terminate_grace_period_seconds"`
	Log                         *coretypes.ServerLogConfig `json:"log" mapstructure:"log"`
	// Metering selects the lifecycle-event recorder backend.
	Metering MeteringConfig `json:"metering,omitzero" mapstructure:"metering"`
}

// Hypervisor returns the selected hypervisor backend type.
func (c *Config) Hypervisor() HypervisorType {
	if c.UseFirecracker {
		return HypervisorFirecracker
	}
	return HypervisorCloudHypervisor
}

// EffectivePoolSize returns PoolSize if set, otherwise runtime.NumCPU().
func (c *Config) EffectivePoolSize() int {
	return utils.PoolSizeOrDefault(c.PoolSize)
}

// EffectivePullConns returns PullConns (defaultPullConns when unset) clamped to maxPullConns.
func (c *Config) EffectivePullConns() int {
	return min(utils.OrDefault(c.PullConns, defaultPullConns), maxPullConns)
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
	if err := c.Metering.Validate(); err != nil {
		return err
	}
	if c.MetaBackend != "" && c.MetaBackend != MetaBackendJSON && c.MetaBackend != MetaBackendSQLite {
		return fmt.Errorf("meta_backend %q is not one of json|sqlite", c.MetaBackend)
	}
	return nil
}

// DNSServers parses the DNS string into a slice of server addresses.
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
