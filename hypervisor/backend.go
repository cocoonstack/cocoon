package hypervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
)

const (
	APISocketName   = "api.sock"
	ConsoleSockName = "console.sock"
	VsockSockName   = "vsock.uds"

	// VsockGuestCID is constant — per-VM isolation comes from distinct UDS paths.
	VsockGuestCID = 3
	// VsockAgentPort is the cocoon-agent listen port.
	VsockAgentPort = 1024

	// CowSerial is the well-known virtio serial for the COW disk attached to OCI VMs.
	CowSerial = "cocoon-cow"
	// COWRawFileName is the raw COW disk's file name in the run dir (single owner so snapshot matchers and clone path rewrites can't drift).
	COWRawFileName = "cow.raw"

	// CreatingStateGCGrace bounds how long GC tolerates a "creating" VM.
	CreatingStateGCGrace = 24 * time.Hour

	// VMMemTransferTimeout is the single-shot timeout for snapshot/restore API calls.
	VMMemTransferTimeout = 10 * time.Minute

	// MinBalloonMemory: balloon overhead is not worthwhile below 256 MiB guest memory.
	MinBalloonMemory = 256 << 20

	// DefaultBalloonDiv sizes the initial balloon as memory/DefaultBalloonDiv (25%).
	DefaultBalloonDiv = 4

	// GracefulStopPollInterval polls between graceful shutdown signal and timeout escalation.
	GracefulStopPollInterval = 500 * time.Millisecond
)

// timeNow is the deterministic-clock seam the differential trace injects (design §10).
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
}

// Backend provides shared store operations for hypervisor backends.
type Backend struct {
	Typ      string
	NS       string
	Conf     BackendConfig
	Meta     meta.Store
	Metering metering.Recorder

	// NetCleanup releases a VM's host networking during delete/recovery.
	NetCleanup NetTeardown
	// NetLife converges host networking inside the VM ops lock on start and stop.
	NetLife NetLifecycle
}

// NewBackend wires shared init: EnsureDirs, the backend's namespace on the
// injected meta store, nil-recorder fallback.
func NewBackend(typ string, conf BackendConfig, rec metering.Recorder, store meta.Store) (*Backend, error) {
	if err := conf.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}
	if rec == nil {
		rec = metering.NopRecorder{}
	}
	return &Backend{
		Typ:      typ,
		NS:       VMNamespaceName(typ),
		Conf:     conf,
		Meta:     store,
		Metering: rec,
	}, nil
}

func (b *Backend) Type() string { return b.Typ }

// SetNetCleanup injects the network teardown used by delete and recovery.
func (b *Backend) SetNetCleanup(fn NetTeardown) { b.NetCleanup = fn }

// SetNetLifecycle injects the locked network convergence used by start, stop and supervision.
func (b *Backend) SetNetLifecycle(nl NetLifecycle) { b.NetLife = nl }

// LaunchSpec is the per-call input to Backend.LaunchVMProcess.
type LaunchSpec struct {
	Cmd       *exec.Cmd
	PIDPath   string
	SockPath  string
	NetnsPath string
	OnFail    func()
}

// RestoreSpec carries backend hooks for Backend.RestoreSequence.
type RestoreSpec struct {
	VMCfg            *types.VMConfig
	Snapshot         io.Reader
	SourceSnapshotID string
	Preflight        func(stagingDir string, rec *VMRecord) error
	Kill             func(ctx context.Context, vmID string, rec *VMRecord) error
	Wrap             func(rec *VMRecord, fn func() error) error
	BeforeMerge      func(rec *VMRecord) error
	AfterExtract     func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *VMRecord) (*types.VM, error)
}

// DirectRestoreSpec is RestoreSpec for a local srcDir; Populate replaces staging+merge.
type DirectRestoreSpec struct {
	VMCfg            *types.VMConfig
	SrcDir           string
	SourceSnapshotID string
	Preflight        func(srcDir string, rec *VMRecord) error
	Kill             func(ctx context.Context, vmID string, rec *VMRecord) error
	Wrap             func(rec *VMRecord, fn func() error) error
	Populate         func(rec *VMRecord, srcDir string) error
	AfterExtract     func(ctx context.Context, vmID string, vmCfg *types.VMConfig, rec *VMRecord) (*types.VM, error)
}

// StartSpec carries StartSequence inputs; Wrap (optional) encloses launch through the Running flip, inside the ops lock.
type StartSpec struct {
	RuntimeFiles []string
	Launch       func(ctx context.Context, rec *VMRecord, sockPath string) (int, error)
	PostLaunch   func(ctx context.Context, rec *VMRecord, sockPath string, pid int) error
	Wrap         func(rec *VMRecord, fn func() error) error
}

// StopSpec carries StopOneSequence inputs.
type StopSpec struct {
	RuntimeFiles []string
	Shutdown     func(ctx context.Context, rec *VMRecord, sockPath string, pid int) error
}

// CreateSpec carries CreateSequence inputs.
type CreateSpec struct {
	VMCfg          *types.VMConfig
	StorageConfigs []*types.StorageConfig
	Net            types.NetSetup
	BootConfig     *types.BootConfig
	Prepare        func(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, net types.NetSetup, boot *types.BootConfig) ([]*types.StorageConfig, error)
}

// SnapshotSpec carries backend hooks for SnapshotSequence; the shared hc keeps HTTP keep-alive across pause/capture/resume.
type SnapshotSpec struct {
	Pause        func(rec *VMRecord, hc *http.Client) error
	Resume       func(rec *VMRecord, hc *http.Client) error
	Capture      func(rec *VMRecord, hc *http.Client, tmpDir string) error
	Wrap         func(rec *VMRecord, fn func() error) error
	AfterCapture func(rec *VMRecord, tmpDir string) error
	BuildMeta    func(rec *VMRecord, tmpDir string) (*SnapshotMeta, error)
}

// HibernateSpec extends SnapshotSpec with the pause-window terminate hook and the backend's runtime files.
type HibernateSpec struct {
	SnapshotSpec
	Terminate    func(rec *VMRecord, hc *http.Client, pid int) error
	RuntimeFiles []string
}

func runWrapped(rec *VMRecord, wrap func(*VMRecord, func() error) error, fn func() error) error {
	if wrap != nil {
		return wrap(rec, fn)
	}
	return fn()
}
