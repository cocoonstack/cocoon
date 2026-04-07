package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/cocoonstack/cocoon/utils"
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

func putNetworkInterface(ctx context.Context, hc *http.Client, iface fcNetworkInterface) error {
	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("marshal network-interface: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/network-interfaces/"+iface.IfaceID, body)
}

func instanceStart(ctx context.Context, hc *http.Client) error {
	body, err := json.Marshal(fcAction{ActionType: "InstanceStart"})
	if err != nil {
		return fmt.Errorf("marshal action: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/actions", body, http.StatusNoContent, http.StatusOK)
}

func sendCtrlAltDel(ctx context.Context, hc *http.Client) error {
	body, err := json.Marshal(fcAction{ActionType: "SendCtrlAltDel"})
	if err != nil {
		return fmt.Errorf("marshal action: %w", err)
	}
	return fcAPI(ctx, hc, http.MethodPut, "/actions", body)
}
