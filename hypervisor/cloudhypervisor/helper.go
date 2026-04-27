package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// chMemoryRestoreMode controls how CH restores guest memory from a snapshot.
type chMemoryRestoreMode string

type chRestoreConfig struct {
	SourceURL         string              `json:"source_url"`
	MemoryRestoreMode chMemoryRestoreMode `json:"memory_restore_mode,omitempty"`
}

const (
	cmdlineFileName = "cmdline"

	// chMemoryRestoreOnDemand uses userfaultfd (UFFD) to lazily page in
	// guest memory from the snapshot file, avoiding a full upfront copy.
	chMemoryRestoreOnDemand chMemoryRestoreMode = "OnDemand"
)

var runtimeFiles = []string{hypervisor.APISocketName, "ch.pid", hypervisor.ConsoleSockName, cmdlineFileName}

// ReverseLayerSerials extracts layer serials, reversed for overlayfs lowerdir.
func ReverseLayerSerials(storageConfigs []*types.StorageConfig) []string {
	var serials []string
	for _, s := range storageConfigs {
		if s.Role == types.StorageRoleLayer {
			serials = append(serials, s.Serial)
		}
	}
	slices.Reverse(serials)
	return serials
}

// validateSnapshotIntegrity is the CH-specific preflight wrapper around
// hypervisor.ValidateSnapshotIntegrity: it runs the common checks, asserts
// the sidecar mirrors snapshot config.json's disk shape, and verifies the
// CH-specific runtime files (state.json + at least one memory-range-*) are
// present so vm.restore won't fail post-kill.
func validateSnapshotIntegrity(srcDir string, sidecar []*types.StorageConfig) error {
	if err := hypervisor.ValidateSnapshotIntegrity(srcDir, sidecar); err != nil {
		return err
	}
	chCfg, _, err := parseCHConfig(filepath.Join(srcDir, "config.json"))
	if err != nil {
		return fmt.Errorf("parse snapshot config: %w", err)
	}
	if len(sidecar) != len(chCfg.Disks) {
		return fmt.Errorf("sidecar/config.json mismatch: %d vs %d disks", len(sidecar), len(chCfg.Disks))
	}
	// writeSnapshotMeta builds the sidecar by walking chCfg.Disks in order, so
	// sidecar[i] and chCfg.Disks[i] must agree on path and readonly. Tampered or
	// imported sidecars whose order drifts would otherwise let patchCHConfig
	// write the wrong path into the wrong disk slot.
	for i, sc := range sidecar {
		if sc.Path != chCfg.Disks[i].Path {
			return fmt.Errorf("sidecar/config.json disk[%d] path mismatch: %q vs %q", i, sc.Path, chCfg.Disks[i].Path)
		}
		if sc.RO != chCfg.Disks[i].ReadOnly {
			return fmt.Errorf("sidecar/config.json disk[%d] readonly mismatch: sidecar=%v config=%v", i, sc.RO, chCfg.Disks[i].ReadOnly)
		}
	}
	if _, statErr := os.Stat(filepath.Join(srcDir, "state.json")); statErr != nil {
		return fmt.Errorf("state.json missing: %w", statErr)
	}
	hasMemory, memErr := hasMemoryRangeFile(srcDir)
	if memErr != nil {
		return fmt.Errorf("read snapshot dir: %w", memErr)
	}
	if !hasMemory {
		return fmt.Errorf("no memory-range-* file in snapshot")
	}
	return nil
}

// hasMemoryRangeFile reports whether srcDir contains at least one CH
// memory-range-* file. We can't enumerate the exact set without parsing
// state.json, but a missing prefix is enough to fail vm.restore.
func hasMemoryRangeFile(srcDir string) (bool, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "memory-range") {
			return true, nil
		}
	}
	return false, nil
}

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

// snapshotVM and restoreVM temporarily extend the client timeout for
// long-running memory transfers, then restore it for subsequent calls.
func snapshotVM(ctx context.Context, hc *http.Client, destDir string) error {
	hc.Timeout = hypervisor.VMMemTransferTimeout
	defer func() { hc.Timeout = utils.HTTPTimeout }()
	body, err := json.Marshal(map[string]string{
		"destination_url": "file://" + destDir,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot request: %w", err)
	}
	_, err = utils.DoAPI(ctx, hc, http.MethodPut,
		"http://localhost/api/v1/vm.snapshot", body, http.StatusNoContent)
	return err
}

func restoreVM(ctx context.Context, hc *http.Client, sourceDir string, onDemand bool) error {
	hc.Timeout = hypervisor.VMMemTransferTimeout
	defer func() { hc.Timeout = utils.HTTPTimeout }()
	cfg := chRestoreConfig{
		SourceURL: "file://" + sourceDir,
	}
	if onDemand {
		cfg.MemoryRestoreMode = chMemoryRestoreOnDemand
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal restore request: %w", err)
	}
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
