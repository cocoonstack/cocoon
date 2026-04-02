package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	apiSockName     = "api.sock"
	pidFileName     = "ch.pid"
	cmdlineFileName = "cmdline"
	consoleSockName = "console.sock"
)

var runtimeFiles = []string{apiSockName, pidFileName, cmdlineFileName, consoleSockName}

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

func snapshotVM(ctx context.Context, hc *http.Client, destDir string) error {
	body, err := json.Marshal(map[string]string{
		"destination_url": "file://" + destDir,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot request: %w", err)
	}
	return vmAPI(ctx, hc, "vm.snapshot", body)
}

func restoreVM(ctx context.Context, hc *http.Client, sourceDir string) error {
	body, err := json.Marshal(map[string]string{
		"source_url": "file://" + sourceDir,
	})
	if err != nil {
		return fmt.Errorf("marshal restore request: %w", err)
	}
	return vmAPI(ctx, hc, "vm.restore", body)
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

// blobHexFromPath extracts the digest hex from a blob file path.
// e.g., "/var/lib/cocoon/oci/blobs/abc123.erofs" → "abc123"
func blobHexFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// forEachVM runs fn for each ID concurrently (bounded by PoolSize),
// collects successes, and logs failures.
// All IDs are attempted (best-effort); errors are logged and collected.
// The returned succeeded slice is always valid, even when err != nil.
func (ch *CloudHypervisor) forEachVM(ctx context.Context, ids []string, op string, fn func(context.Context, string) error) ([]string, error) {
	logger := log.WithFunc("cloudhypervisor." + op)
	result := utils.ForEach(ctx, ids, fn, ch.conf.EffectivePoolSize())
	for _, err := range result.Errors {
		logger.Warnf(ctx, "%s: %v", op, err)
	}
	return result.Succeeded, result.Err()
}

func toVM(rec *hypervisor.VMRecord) *types.VM {
	info := rec.VM // value copy — detached from the DB record
	if info.State == types.VMStateRunning {
		info.SocketPath = socketPath(rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(pidFile(rec.RunDir))
	}
	return &info
}

// socketPath returns the API socket path under a VM's run directory.
func socketPath(runDir string) string { return filepath.Join(runDir, apiSockName) }

// pidFile returns the PID file path under a VM's run directory.
func pidFile(runDir string) string { return filepath.Join(runDir, pidFileName) }

// consoleSockPath returns the console socket path under a VM's run directory.
func consoleSockPath(runDir string) string { return filepath.Join(runDir, consoleSockName) }

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

func cleanupRuntimeFiles(ctx context.Context, runDir string) {
	for _, name := range runtimeFiles {
		p := filepath.Join(runDir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.WithFunc("cloudhypervisor.cleanupRuntimeFiles").Warnf(ctx, "cleanup %s: %v", p, err)
		}
	}
}

// qemuExpandImage expands a disk image to targetSize if its current virtual
// size is smaller. For raw/sparse files (directBoot), os.Truncate is used;
// for qcow2 images, qemu-img resize is used. No-op if already large enough.
func qemuExpandImage(ctx context.Context, path string, targetSize int64, directBoot bool) error {
	if directBoot {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if targetSize <= fi.Size() {
			return nil
		}
		if err := os.Truncate(path, targetSize); err != nil {
			return fmt.Errorf("truncate %s to %d: %w", path, targetSize, err)
		}
		return nil
	}

	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path).Output() //nolint:gosec
	if err != nil {
		return fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return fmt.Errorf("parse qemu-img info %s: %w", path, err)
	}
	if targetSize <= info.VirtualSize {
		return nil
	}
	if out, err := exec.CommandContext(ctx, //nolint:gosec
		"qemu-img", "resize", path, fmt.Sprintf("%d", targetSize),
	).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize %s: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func removeVMDirs(runDir, logDir string) error {
	return errors.Join(
		os.RemoveAll(runDir),
		os.RemoveAll(logDir),
	)
}
