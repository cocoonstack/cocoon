package hypervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	APISocketName   = "api.sock"
	ConsoleSockName = "console.sock"
	ConsolePTYName  = "console.pty"
	VsockSockName   = "vsock.uds"

	// VsockGuestCID is constant — per-VM isolation comes from distinct UDS paths.
	VsockGuestCID = 3
	// VsockAgentPort is the cocoon-agent listen port.
	VsockAgentPort = 1024

	// CowSerial is the well-known virtio serial for the COW disk attached to OCI VMs.
	CowSerial = "cocoon-cow"

	// StaleArtifactGCGrace ages leftover capture/staging dirs and clone locks.
	StaleArtifactGCGrace = 24 * time.Hour

	// VMMemTransferTimeout is the single-shot timeout for snapshot/restore API calls.
	VMMemTransferTimeout = 10 * time.Minute

	// MinBalloonMemory is the floor below which balloon overhead is not worthwhile.
	MinBalloonMemory = 256 << 20

	DefaultBalloonDiv = 4
)

// timeNow is the clock seam tests inject.
var timeNow = time.Now

// BackendConfig provides backend-specific values needed by shared Backend methods.
type BackendConfig interface {
	BinaryName() string
	RootDirPath() string
	PIDFileName() string
	TerminateGracePeriod() time.Duration
	SocketWaitTimeout() time.Duration
	EffectivePoolSize() int
	EnsureDirs() error
	RunDir() string
	LogDir() string
	VMRunDir(id string) string
	VMLogDir(id string) string
	CgroupParentDir() string
	CgroupCPUFence() string
}

// LaunchSpec is the per-call input to Backend.LaunchVMProcess; PID-file and socket paths derive from Rec.RunDir.
type LaunchSpec struct {
	Cmd       *exec.Cmd
	NetnsPath string
	OnFail    func()

	// Rec names the VM whose CPU scope the process enters at spawn (ID + cgroup knobs); every VM enters a scope.
	Rec *VMRecord

	// DeferCPUQuota leaves the scope's ceiling at max through the paused provisioning window; the backend arms the finite quota before resume.
	DeferCPUQuota bool
}

// VMOp is one backend's per-VM lifecycle step, run by ForEachVM under the batch fan-out.
type VMOp func(context.Context, string) error

// StartOp is a backend's per-VM start, handed the batch's one /proc scan for its liveness check.
type StartOp func(ctx context.Context, id string, scan *utils.ProcScan) error

// PreflightHook validates rec against the snapshot source dir before anything is applied.
type PreflightHook func(dir string, rec *VMRecord) error

// KillHook stops the origin VM process before the destructive phase rewrites the run dir.
type KillHook func(ctx context.Context, vmID string, rec *VMRecord) error

// PrepareHook lays out a VM's disks before launch and returns the final storage set.
type PrepareHook func(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, net types.NetSetup, boot *types.BootConfig) ([]*types.StorageConfig, error)

// AfterExtractHook finalizes the restored record and returns the resulting VM.
type AfterExtractHook func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *VMRecord) (*types.VM, error)

type ProcessHook func(ctx context.Context, rec *VMRecord, sockPath string, pid int) error

type TransitionHook func(rec *VMRecord, hc *http.Client) error

// RestoreSpec carries backend hooks for Backend.RestoreSequence.
type RestoreSpec struct {
	VMCfg            *types.VMConfig
	Snapshot         io.Reader
	SourceSnapshotID string
	Preflight        PreflightHook
	Kill             KillHook
	BeforeMerge      func(rec *VMRecord) error
	AfterExtract     AfterExtractHook
}

// DirectRestoreSpec is RestoreSpec for a local srcDir; Populate replaces staging+merge.
type DirectRestoreSpec struct {
	VMCfg            *types.VMConfig
	SrcDir           string
	SourceSnapshotID string
	Preflight        PreflightHook
	Kill             KillHook
	Populate         func(rec *VMRecord, srcDir string) error
	AfterExtract     AfterExtractHook
}

// StartSpec carries StartSequence inputs.
type StartSpec struct {
	RuntimeFiles []string
	Launch       func(ctx context.Context, rec *VMRecord, sockPath string) (int, error)
	PostLaunch   ProcessHook
}

// StopSpec carries StopOneSequence inputs.
type StopSpec struct {
	RuntimeFiles []string
	Shutdown     ProcessHook
}

// CreateSpec carries CreateSequence inputs.
type CreateSpec struct {
	VMCfg          *types.VMConfig
	StorageConfigs []*types.StorageConfig
	Net            types.NetSetup
	BootConfig     *types.BootConfig
	Prepare        PrepareHook
}

// SnapshotSpec carries backend hooks for SnapshotSequence; the shared hc keeps HTTP keep-alive across pause/capture/resume.
type SnapshotSpec struct {
	Pause        TransitionHook
	Resume       TransitionHook
	Capture      func(rec *VMRecord, hc *http.Client, tmpDir string) error
	AfterCapture func(rec *VMRecord, tmpDir string) error
	BuildMeta    func(rec *VMRecord, tmpDir string) (*SnapshotMeta, error)
}

// HibernateSpec extends SnapshotSpec with the pause-window terminate hook and the backend's runtime files.
type HibernateSpec struct {
	SnapshotSpec
	Terminate    func(rec *VMRecord, hc *http.Client, pid int) error
	RuntimeFiles []string
}

var _ Supervisable = (*Backend)(nil)

// Backend provides shared store operations for hypervisor backends.
type Backend struct {
	Typ      string
	NS       string
	Conf     BackendConfig
	Meta     meta.Store
	Metering metering.Recorder

	// Net converges host networking inside the VM ops lock; nil disables it.
	Net VMNetwork

	// PinsQueues marks a backend that pins writable disk queue threads, so launch derives their host cpus.
	PinsQueues bool
	// PeerNS lists the other backends' VM namespaces; placement counts their placements too.
	PeerNS []string

	// topo memoizes the sysfs cache-domain walk: host topology is constant for the process and cpu hotplug is not tracked.
	topoOnce sync.Once
	topo     *cgroup.Topology
	topoErr  error
}

// NewBackend wires EnsureDirs, the backend's namespace on the injected meta store and the nil-recorder fallback.
func NewBackend(typ string, conf BackendConfig, rec metering.Recorder, store meta.Store) (*Backend, error) {
	if err := conf.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	if rec == nil {
		rec = metering.NopRecorder{}
	}
	b := &Backend{
		Typ:      typ,
		NS:       VMNamespaceName(typ),
		Conf:     conf,
		Meta:     store,
		Metering: rec,
	}
	for _, t := range []config.HypervisorType{config.HypervisorCloudHypervisor, config.HypervisorFirecracker} {
		if ns := VMNamespaceName(string(t)); ns != b.NS {
			b.PeerNS = append(b.PeerNS, ns)
		}
	}
	return b, nil
}

func (b *Backend) Type() string { return b.Typ }
