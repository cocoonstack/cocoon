package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	actionInstanceStart  = "InstanceStart"
	actionSendCtrlAltDel = "SendCtrlAltDel"
	vmStatePaused        = "Paused"
	vmStateResumed       = "Resumed"
	memBackendTypeFile   = "File"

	driveIDFmt = "drive_%d"
	ifaceIDFmt = "eth%d"

	ioEngineAsync = "Async" // io_uring
)

// fcMachineConfig and the request types below follow Firecracker's pre-boot config model: start empty, configure via PUT/PATCH, then InstanceStart.
type fcMachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	InitrdPath      string `json:"initrd_path,omitempty"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	IoEngine     string `json:"io_engine,omitempty"`
}

type fcNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type fcAction struct {
	ActionType string `json:"action_type"`
}

type fcBalloon struct {
	AmountMiB         int  `json:"amount_mib"`
	DeflateOnOOM      bool `json:"deflate_on_oom,omitempty"`
	FreePageReporting bool `json:"free_page_reporting,omitempty"`
}

type fcVsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type fcSnapshotCreate struct {
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type fcSnapshotLoad struct {
	SnapshotPath     string              `json:"snapshot_path"`
	MemBackend       fcSnapshotMemBE     `json:"mem_backend"`
	NetworkOverrides []fcNetworkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *fcVsockOverride    `json:"vsock_override,omitempty"`
}

// fcNetworkOverride overrides a network interface from the snapshot with a new TAP device (FC v1.14+, PR #4731).
type fcNetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// fcVsockOverride retargets the vsock UDS during snapshot/load. Pointer+omitempty keeps the field off the wire for FC < v1.16.
type fcVsockOverride struct {
	UDSPath string `json:"uds_path"`
}

type fcSnapshotMemBE struct {
	BackendPath string `json:"backend_path"`
	BackendType string `json:"backend_type"`
}

// fcAPI PUTs body to an idempotent FC REST endpoint with retry; expects 204.
func fcAPI(ctx context.Context, hc *http.Client, endpoint string, body []byte) error {
	_, err := utils.DoAPIWithRetry(ctx, hc, http.MethodPut, "http://localhost"+endpoint, body)
	return err
}

// putJSON marshals payload and PUTs it to an idempotent FC endpoint with retry.
func putJSON[T any](ctx context.Context, hc *http.Client, endpoint string, payload T, kind string) error {
	_, err := utils.DoJSONWithRetry(ctx, hc, http.MethodPut, "http://localhost"+endpoint, kind, payload)
	return err
}

// sendJSONOnce is putJSON's no-retry twin for non-idempotent state transitions (instance-start, pause/resume) — retry would hit wrong-state.
func sendJSONOnce[T any](ctx context.Context, hc *http.Client, method, endpoint string, payload T, kind string, successCodes ...int) error {
	_, err := utils.DoJSONOnce(ctx, hc, method, "http://localhost"+endpoint, kind, payload, successCodes...)
	return err
}

func putMachineConfig(ctx context.Context, hc *http.Client, cfg fcMachineConfig) error {
	return putJSON(ctx, hc, "/machine-config", cfg, "machine-config")
}

// patchDrivePath repoints a block device's backing file on a booted VM.
func patchDrivePath(ctx context.Context, hc *http.Client, driveID, pathOnHost string) error {
	return sendJSONOnce(ctx, hc, http.MethodPatch, "/drives/"+driveID, struct {
		DriveID    string `json:"drive_id"`
		PathOnHost string `json:"path_on_host"`
	}{DriveID: driveID, PathOnHost: pathOnHost}, "drive patch")
}

func putBootSource(ctx context.Context, hc *http.Client, boot fcBootSource) error {
	return putJSON(ctx, hc, "/boot-source", boot, "boot-source")
}

func putDrive(ctx context.Context, hc *http.Client, drive fcDrive) error {
	return putJSON(ctx, hc, "/drives/"+drive.DriveID, drive, "drive")
}

func putBalloon(ctx context.Context, hc *http.Client, balloon fcBalloon) error {
	return putJSON(ctx, hc, "/balloon", balloon, "balloon")
}

func putNetworkInterface(ctx context.Context, hc *http.Client, iface fcNetworkInterface) error {
	return putJSON(ctx, hc, "/network-interfaces/"+iface.IfaceID, iface, "network-interface")
}

func putEntropy(ctx context.Context, hc *http.Client) error {
	return fcAPI(ctx, hc, "/entropy", []byte("{}"))
}

func putVsock(ctx context.Context, hc *http.Client, vsock fcVsock) error {
	return putJSON(ctx, hc, "/vsock", vsock, "vsock")
}

func instanceStart(ctx context.Context, hc *http.Client) error {
	return sendJSONOnce(ctx, hc, http.MethodPut, "/actions", fcAction{ActionType: actionInstanceStart}, "action",
		http.StatusNoContent, http.StatusOK)
}

func sendCtrlAltDel(ctx context.Context, hc *http.Client) error {
	return sendJSONOnce(ctx, hc, http.MethodPut, "/actions", fcAction{ActionType: actionSendCtrlAltDel}, "action")
}

// pauseVM pauses a running FC instance via PATCH /vm. Idempotent: FC's vCPU event loop acks Pause from the paused state without error (vstate/vcpu.rs).
func pauseVM(ctx context.Context, hc *http.Client) error {
	return sendJSONOnce(ctx, hc, http.MethodPatch, "/vm", map[string]string{"state": vmStatePaused}, "pause request")
}

// resumeVM resumes a paused FC instance via PATCH /vm. Idempotent like pauseVM.
func resumeVM(ctx context.Context, hc *http.Client) error {
	return sendJSONOnce(ctx, hc, http.MethodPatch, "/vm", map[string]string{"state": vmStateResumed}, "resume request")
}

// createSnapshotFC writes vmstate + memory to destDir; no retry — resending would re-transfer multi-GiB and clobber a partial state.json.
func createSnapshotFC(ctx context.Context, sockPath, destDir string) error {
	body, err := json.Marshal(fcSnapshotCreate{
		SnapshotPath: filepath.Join(destDir, snapshotVMStateFile),
		MemFilePath:  filepath.Join(destDir, snapshotMemFile),
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot/create request: %w", err)
	}
	hc := utils.NewSocketHTTPClientWithTimeout(sockPath, hypervisor.VMMemTransferTimeout)
	_, err = utils.DoAPIOnce(ctx, hc, http.MethodPut,
		"http://localhost/snapshot/create", body, http.StatusNoContent)
	return err
}

// loadSnapshotFC loads from sourceDir into a fresh FC; vsockUDSOverride="" inherits the snapshot's path (FC < v1.16). No retry (same reason as createSnapshotFC).
func loadSnapshotFC(ctx context.Context, sockPath, sourceDir string, networkOverrides []fcNetworkOverride, vsockUDSOverride string) error {
	req := fcSnapshotLoad{
		SnapshotPath: filepath.Join(sourceDir, snapshotVMStateFile),
		MemBackend: fcSnapshotMemBE{
			BackendPath: filepath.Join(sourceDir, snapshotMemFile),
			BackendType: memBackendTypeFile,
		},
		NetworkOverrides: networkOverrides,
	}
	if vsockUDSOverride != "" {
		req.VsockOverride = &fcVsockOverride{UDSPath: vsockUDSOverride}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal snapshot/load request: %w", err)
	}
	hc := utils.NewSocketHTTPClientWithTimeout(sockPath, hypervisor.VMMemTransferTimeout)
	_, err = utils.DoAPIOnce(ctx, hc, http.MethodPut,
		"http://localhost/snapshot/load", body, http.StatusNoContent)
	return err
}
