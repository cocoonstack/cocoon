package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/cocoonstack/cocoon/utils"
)

// FC REST API constants.
const (
	actionInstanceStart  = "InstanceStart"
	actionSendCtrlAltDel = "SendCtrlAltDel"
	vmStatePaused        = "Paused"
	vmStateResumed       = "Resumed"
	memBackendTypeFile   = "File"

	driveIDFmt = "drive_%d"
	ifaceIDFmt = "eth%d"

	cowFileName = "cow.raw"
)

// Firecracker REST API request types.
// FC uses a pre-boot configuration model: start an empty process,
// configure via HTTP PUT/PATCH, then issue InstanceStart.

type fcMachineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	HugePages  bool `json:"hugepages,omitempty"`
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

// fcAPI sends a request to the Firecracker REST API via Unix socket.
func fcAPI(ctx context.Context, hc *http.Client, method, endpoint string, body []byte, successCodes ...int) error { //nolint:unparam
	if len(successCodes) == 0 {
		successCodes = []int{http.StatusNoContent}
	}
	primaryCode := successCodes[0]

	_, err := utils.DoWithRetry(ctx, func() ([]byte, error) {
		resp, apiErr := utils.DoAPI(ctx, hc, method, "http://localhost"+endpoint, body, primaryCode)
		if apiErr == nil {
			return resp, nil
		}
		var ae *utils.APIError
		if errors.As(apiErr, &ae) && slices.Contains(successCodes[1:], ae.Code) {
			return nil, nil
		}
		return nil, apiErr
	})
	return err
}

func putMachineConfig(ctx context.Context, hc *http.Client, cfg fcMachineConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal machine-config: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/machine-config", body)
}

func putBootSource(ctx context.Context, hc *http.Client, boot fcBootSource) error {
	body, err := json.Marshal(boot)
	if err != nil {
		return fmt.Errorf("marshal boot-source: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/boot-source", body)
}

func putDrive(ctx context.Context, hc *http.Client, drive fcDrive) error {
	body, err := json.Marshal(drive)
	if err != nil {
		return fmt.Errorf("marshal drive: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/drives/"+drive.DriveID, body)
}

func putBalloon(ctx context.Context, hc *http.Client, balloon fcBalloon) error {
	body, err := json.Marshal(balloon)
	if err != nil {
		return fmt.Errorf("marshal balloon: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/balloon", body)
}

func putNetworkInterface(ctx context.Context, hc *http.Client, iface fcNetworkInterface) error {
	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("marshal network-interface: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/network-interfaces/"+iface.IfaceID, body)
}

func instanceStart(ctx context.Context, hc *http.Client) error {
	body, err := json.Marshal(fcAction{ActionType: actionInstanceStart})
	if err != nil {
		return fmt.Errorf("marshal action: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/actions", body, http.StatusNoContent, http.StatusOK)
}

func sendCtrlAltDel(ctx context.Context, hc *http.Client) error {
	body, err := json.Marshal(fcAction{ActionType: actionSendCtrlAltDel})
	if err != nil {
		return fmt.Errorf("marshal action: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/actions", body)
}

// pauseVM pauses a running FC instance via PATCH /vm.
func pauseVM(ctx context.Context, hc *http.Client) error {
	body, err := json.Marshal(map[string]string{"state": vmStatePaused})
	if err != nil {
		return fmt.Errorf("marshal pause request: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPatch, "/vm", body)
}

// resumeVM resumes a paused FC instance via PATCH /vm.
func resumeVM(ctx context.Context, hc *http.Client) error {
	body, err := json.Marshal(map[string]string{"state": vmStateResumed})
	if err != nil {
		return fmt.Errorf("marshal resume request: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPatch, "/vm", body)
}

// FC snapshot/load request types.
type fcSnapshotCreate struct {
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type fcSnapshotLoad struct {
	SnapshotPath        string          `json:"snapshot_path"`
	MemBackend          fcSnapshotMemBE `json:"mem_backend"`
	EnableDiffSnapshots bool            `json:"enable_diff_snapshots,omitempty"`
	ResumeVM            bool            `json:"resume_vm,omitempty"`
}

type fcSnapshotMemBE struct {
	BackendPath string `json:"backend_path"`
	BackendType string `json:"backend_type"`
}

// createSnapshotFC creates a full VM snapshot (vmstate + memory file) in destDir.
func createSnapshotFC(ctx context.Context, hc *http.Client, destDir string) error {
	body, err := json.Marshal(fcSnapshotCreate{
		SnapshotPath: filepath.Join(destDir, snapshotVMStateFile),
		MemFilePath:  filepath.Join(destDir, snapshotMemFile),
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot/create request: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/snapshot/create", body)
}

// loadSnapshotFC loads a VM snapshot from sourceDir into a freshly started FC process.
func loadSnapshotFC(ctx context.Context, hc *http.Client, sourceDir string) error {
	body, err := json.Marshal(fcSnapshotLoad{
		SnapshotPath: filepath.Join(sourceDir, snapshotVMStateFile),
		MemBackend: fcSnapshotMemBE{
			BackendPath: filepath.Join(sourceDir, snapshotMemFile),
			BackendType: memBackendTypeFile,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot/load request: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/snapshot/load", body)
}
