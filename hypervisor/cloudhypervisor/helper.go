package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const cmdlineFileName = "cmdline"

var runtimeFiles = []string{hypervisor.APISocketName, "ch.pid", hypervisor.ConsoleSockName, cmdlineFileName}

// ReverseLayerSerials extracts read-only layer serial names from StorageConfigs
// and returns them in reverse order (top layer first for overlayfs lowerdir).
func ReverseLayerSerials(storageConfigs []*types.StorageConfig) []string {
	var serials []string
	for _, s := range storageConfigs {
		if s.RO {
			serials = append(serials, s.Serial)
		}
	}
	slices.Reverse(serials)
	return serials
}

// vmAPI sends a PUT request to a Cloud Hypervisor REST API endpoint.
// Reuses the provided http.Client to avoid creating a new client per call.
func vmAPI(ctx context.Context, hc *http.Client, endpoint string, body []byte, successCodes ...int) error {
	if len(successCodes) == 0 {
		successCodes = []int{http.StatusNoContent}
	}
	primaryCode := successCodes[0]

	_, err := utils.DoWithRetry(ctx, func() ([]byte, error) {
		resp, apiErr := utils.DoAPI(ctx, hc, http.MethodPut, "http://localhost/api/v1/"+endpoint, body, primaryCode)
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

func shutdownVM(ctx context.Context, hc *http.Client) error {
	return vmAPI(ctx, hc, "vm.shutdown", nil)
}

func pauseVM(ctx context.Context, hc *http.Client) error {
	return vmAPI(ctx, hc, "vm.pause", nil)
}

func resumeVM(ctx context.Context, hc *http.Client) error {
	return vmAPI(ctx, hc, "vm.resume", nil)
}

// snapshotVM and restoreVM bypass vmAPI's retry layer — see hypervisor.VMMemTransferTimeout.
func snapshotVM(ctx context.Context, sockPath, destDir string) error {
	body, err := json.Marshal(map[string]string{
		"destination_url": "file://" + destDir,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot request: %w", err)
	}
	hc := utils.NewSocketHTTPClientWithTimeout(sockPath, hypervisor.VMMemTransferTimeout)
	_, err = utils.DoAPI(ctx, hc, http.MethodPut,
		"http://localhost/api/v1/vm.snapshot", body, http.StatusNoContent)
	return err
}

func restoreVM(ctx context.Context, sockPath, sourceDir string) error {
	body, err := json.Marshal(map[string]string{
		"source_url": "file://" + sourceDir,
	})
	if err != nil {
		return fmt.Errorf("marshal restore request: %w", err)
	}
	hc := utils.NewSocketHTTPClientWithTimeout(sockPath, hypervisor.VMMemTransferTimeout)
	_, err = utils.DoAPI(ctx, hc, http.MethodPut,
		"http://localhost/api/v1/vm.restore", body, http.StatusNoContent)
	return err
}

func addDiskVM(ctx context.Context, hc *http.Client, disk chDisk) error {
	body, err := json.Marshal(disk)
	if err != nil {
		return fmt.Errorf("marshal add-disk request: %w", err)
	}
	return vmAPI(ctx, hc, "vm.add-disk", body, http.StatusOK, http.StatusNoContent)
}

func removeDeviceVM(ctx context.Context, hc *http.Client, deviceID string) error {
	body, err := json.Marshal(map[string]string{"id": deviceID})
	if err != nil {
		return fmt.Errorf("marshal remove-device request: %w", err)
	}
	return vmAPI(ctx, hc, "vm.remove-device", body)
}

func addNetVM(ctx context.Context, hc *http.Client, net chNet) error {
	body, err := json.Marshal(net)
	if err != nil {
		return fmt.Errorf("marshal add-net request: %w", err)
	}
	return vmAPI(ctx, hc, "vm.add-net", body, http.StatusOK, http.StatusNoContent)
}

func powerButton(ctx context.Context, hc *http.Client) error {
	return vmAPI(ctx, hc, "vm.power-button", nil)
}

// queryConsolePTY retrieves the virtio-console PTY path from a running CH instance
// via GET /api/v1/vm.info. Returns empty string if the console is not in Pty mode.
func queryConsolePTY(ctx context.Context, apiSocketPath string) (string, error) {
	hc := utils.NewSocketHTTPClient(apiSocketPath)
	body, err := utils.DoAPI(ctx, hc, http.MethodGet, "http://localhost/api/v1/vm.info", nil, http.StatusOK)
	if err != nil {
		return "", fmt.Errorf("query vm.info: %w", err)
	}
	var info chVMInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("decode vm.info: %w", err)
	}
	if info.Config.Console.File == "" {
		return "", fmt.Errorf("console PTY not available (mode=%s)", info.Config.Console.Mode)
	}
	return info.Config.Console.File, nil
}

// resolveConsole determines the console path for a VM after launch.
// Direct-boot (OCI) VMs use a PTY allocated by CH; UEFI VMs use a Unix socket.
func resolveConsole(ctx context.Context, vmID, sockPath, consoleSock string, directBoot bool) string {
	if directBoot {
		consolePath, err := utils.DoWithRetry(ctx, func() (string, error) {
			return queryConsolePTY(ctx, sockPath)
		})
		if err != nil {
			log.WithFunc("cloudhypervisor.resolveConsole").Warnf(ctx, "query console PTY for %s: %v", vmID, err)
		}
		return consolePath
	}
	return consoleSock
}
