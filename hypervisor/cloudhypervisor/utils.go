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
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	pidFileName     = "ch.pid"
	cmdlineFileName = "cmdline"
	configJSONName  = "config.json"
	stateJSONName   = "state.json"
	memoryRangeFile = "memory-range" // prefix shared by all per-region memory-range-* files in a CH snapshot

	chAPIBase     = "http://localhost/api/v1/"
	apiSocketFlag = "--api-socket"

	restoreModeCopy     = "copy"
	restoreModeOnDemand = "ondemand"
	restoreModeMmap     = "mmap"

	// chMemoryRestoreOnDemand lazily pages in guest memory via userfaultfd instead of a full upfront copy.
	chMemoryRestoreOnDemand chMemoryRestoreMode = "OnDemand"
	// chMemoryRestoreMmap maps the snapshot file copy-on-write, sharing page cache across clones of one snapshot.
	chMemoryRestoreMmap chMemoryRestoreMode = "CopyOnWrite"
)

var runtimeFiles = []string{hypervisor.APISocketName, pidFileName, hypervisor.ConsoleSockName, hypervisor.ConsolePTYName, cmdlineFileName, hypervisor.VsockSockName}

type chMemoryRestoreMode string

type chRestoreConfig struct {
	SourceURL         string              `json:"source_url"`
	MemoryRestoreMode chMemoryRestoreMode `json:"memory_restore_mode,omitempty"`
}

func (ch *CloudHypervisor) saveCmdline(ctx context.Context, rec *hypervisor.VMRecord, args []string) {
	line := ch.conf.CHBinary + " " + strings.Join(args, " ")
	if err := utils.AtomicWriteFile(filepath.Join(rec.RunDir, cmdlineFileName), []byte(line), 0o600, utils.NoSync); err != nil {
		log.WithFunc("cloudhypervisor.saveCmdline").Warnf(ctx, "save cmdline: %v", err)
	}
}

func (ch *CloudHypervisor) cowPath(vmID string, directBoot bool) string {
	if directBoot {
		return ch.conf.COWRawPath(vmID)
	}
	return ch.conf.OverlayPath(vmID)
}

// ReverseLayerSerials extracts layer serials, reversed for overlayfs lowerdir.
func ReverseLayerSerials(storageConfigs []*types.StorageConfig) []string {
	return hypervisor.ReverseLayers(storageConfigs, func(_ int, sc *types.StorageConfig) string { return sc.Serial })
}

// callers pass their parsed config.json so clone and restore decode it once.
func validateSnapshotIntegrityParsed(srcDir string, sidecar []*types.StorageConfig, chCfg *chVMConfig) error {
	if err := hypervisor.ValidateSnapshotIntegrity(srcDir, sidecar); err != nil {
		return err
	}
	if len(sidecar) != len(chCfg.Disks) {
		return fmt.Errorf("sidecar/config.json mismatch: %d vs %d disks", len(sidecar), len(chCfg.Disks))
	}
	// sidecar[i] must agree with chCfg.Disks[i] (path, readonly); drift would let patchCHConfig write to the wrong slot.
	for i, sc := range sidecar {
		if sc.Path != chCfg.Disks[i].Path {
			return fmt.Errorf("sidecar/config.json disk[%d] path mismatch: %q vs %q", i, sc.Path, chCfg.Disks[i].Path)
		}
		if sc.RO != chCfg.Disks[i].ReadOnly {
			return fmt.Errorf("sidecar/config.json disk[%d] readonly mismatch: sidecar=%v config=%v", i, sc.RO, chCfg.Disks[i].ReadOnly)
		}
	}
	if _, statErr := os.Stat(filepath.Join(srcDir, stateJSONName)); statErr != nil {
		return fmt.Errorf("state.json missing: %w", statErr)
	}
	return requireMemoryRangeFile(srcDir)
}

// a missing memory-range-* prefix alone is enough to fail vm.restore.
func requireMemoryRangeFile(srcDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read snapshot dir: %w", err)
	}
	if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), memoryRangeFile) }) {
		return fmt.Errorf("no memory-range-* file in snapshot")
	}
	return nil
}

// vmAPIOnce never retries: the CH endpoints it serves are not idempotent.
func vmAPIOnce(ctx context.Context, hc *http.Client, endpoint string, body []byte, successCodes ...int) ([]byte, error) {
	return utils.DoAPIOnce(ctx, hc, http.MethodPut, chAPIBase+endpoint, body, successCodes...)
}

func vmPutJSON[T any](ctx context.Context, hc *http.Client, endpoint, kind string, payload T, successCodes ...int) error {
	_, err := utils.DoJSONOnce(ctx, hc, http.MethodPut, chAPIBase+endpoint, kind, payload, successCodes...)
	return err
}

func shutdownVM(ctx context.Context, hc *http.Client) error {
	_, err := vmAPIOnce(ctx, hc, "vm.shutdown", nil)
	return err
}

// pauseVM is idempotent — swallows CH's Paused→Paused 500 so a stuck-paused VM recovers.
func pauseVM(ctx context.Context, hc *http.Client) error {
	_, err := vmAPIOnce(ctx, hc, "vm.pause", nil)
	if err != nil && isAlreadyInStateError(err, "Paused") {
		return nil
	}
	return err
}

// resumeVM is idempotent — swallows CH's Running→Running 500.
func resumeVM(ctx context.Context, hc *http.Client) error {
	_, err := vmAPIOnce(ctx, hc, "vm.resume", nil)
	if err != nil && isAlreadyInStateError(err, "Running") {
		return nil
	}
	return err
}

// isAlreadyInStateError matches CH's exact `Invalid transition: InvalidStateTransition(<state>, <state>)` in a 500 body.
func isAlreadyInStateError(err error, state string) bool {
	ae, ok := errors.AsType[*utils.APIError](err)
	if !ok || ae.Code != http.StatusInternalServerError {
		return false
	}
	return strings.Contains(ae.Message, fmt.Sprintf("Invalid transition: InvalidStateTransition(%s, %s)", state, state))
}

// the memory transfer needs a longer timeout than the shared client default.
func snapshotVM(ctx context.Context, hc *http.Client, destDir string) error {
	hc.Timeout = hypervisor.VMMemTransferTimeout
	defer func() { hc.Timeout = utils.HTTPTimeout }()
	body, err := json.Marshal(map[string]string{
		"destination_url": "file://" + destDir,
	})
	if err != nil {
		return fmt.Errorf("marshal snapshot request: %w", err)
	}
	_, err = utils.DoAPIOnce(ctx, hc, http.MethodPut,
		chAPIBase+"vm.snapshot", body, http.StatusNoContent)
	return err
}

func restoreVM(ctx context.Context, hc *http.Client, sourceDir, restoreMode string) error {
	hc.Timeout = hypervisor.VMMemTransferTimeout
	defer func() { hc.Timeout = utils.HTTPTimeout }()
	cfg := chRestoreConfig{
		SourceURL: "file://" + sourceDir,
	}
	switch restoreMode {
	case "", restoreModeCopy: // eager copy is CH's default; leave MemoryRestoreMode unset
	case restoreModeOnDemand:
		cfg.MemoryRestoreMode = chMemoryRestoreOnDemand
	case restoreModeMmap:
		cfg.MemoryRestoreMode = chMemoryRestoreMmap
	default:
		// Fail loud: a misspelled mode silently falling back to eager copy is a ~6x restore-latency regression.
		return fmt.Errorf("unknown restore mode %q", restoreMode)
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal restore request: %w", err)
	}
	_, err = utils.DoAPIOnce(ctx, hc, http.MethodPut,
		chAPIBase+"vm.restore", body, http.StatusNoContent)
	return err
}

// addDiskVM is non-idempotent — a retry after a successful attach would surface as "duplicate id".
func addDiskVM(ctx context.Context, hc *http.Client, disk chDisk) error {
	return vmPutJSON(ctx, hc, "vm.add-disk", "add-disk request", disk, http.StatusOK, http.StatusNoContent)
}

// removeDeviceVM is non-idempotent — a retry after a lost ACK would surface as "id not found".
func removeDeviceVM(ctx context.Context, hc *http.Client, deviceID string) error {
	return vmPutJSON(ctx, hc, "vm.remove-device", "remove-device request", map[string]string{"id": deviceID})
}

// waitDeviceEjected blocks until id is gone from CH's device_tree (bounded by ejectWaitTimeout: Linux acks B0EJ < 1 s, Windows can take 10–20 s).
func waitDeviceEjected(ctx context.Context, hc *http.Client, deviceID string) error {
	return utils.WaitFor(ctx, ejectWaitTimeout, 100*time.Millisecond, func() (bool, error) {
		info, err := getVMInfo(ctx, hc)
		if err != nil {
			return false, err
		}
		_, present := info.DeviceTree[deviceID]
		return !present, nil
	})
}

func addNetVM(ctx context.Context, hc *http.Client, net chNet) error {
	return vmPutJSON(ctx, hc, "vm.add-net", "add-net request", net, http.StatusOK, http.StatusNoContent)
}

// addCocoonNIC posts vm.add-net with the deterministic cocoon-net-<mac> id; returns id for rollback.
func addCocoonNIC(ctx context.Context, hc *http.Client, nc *types.NetworkConfig) (string, error) {
	chN := networkConfigToNet(nc)
	chN.ID = cocoonNetID(nc.MAC)
	if err := addNetVM(ctx, hc, chN); err != nil {
		return "", err
	}
	return chN.ID, nil
}

func getVMInfo(ctx context.Context, hc *http.Client) (*chVMInfoResponse, error) {
	body, err := utils.DoAPI(ctx, hc, http.MethodGet, chAPIBase+"vm.info", nil, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("query vm.info: %w", err)
	}
	var info chVMInfoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode vm.info: %w", err)
	}
	return &info, nil
}

func decodePciDeviceInfo(resp []byte) (chPciDeviceInfo, error) {
	if len(resp) == 0 {
		return chPciDeviceInfo{}, nil
	}
	var info chPciDeviceInfo
	if err := json.Unmarshal(resp, &info); err != nil {
		return chPciDeviceInfo{}, fmt.Errorf("decode PciDeviceInfo: %w", err)
	}
	return info, nil
}

func powerButton(ctx context.Context, hc *http.Client) error {
	_, err := vmAPIOnce(ctx, hc, "vm.power-button", nil)
	return err
}

// confirmVMMReady proves the VMM answers its API: cloud-hypervisor binds the API socket before it validates its launch config.
func confirmVMMReady(ctx context.Context, hc *http.Client) error {
	if _, err := utils.DoWithRetry(ctx, func() (*chVMInfoResponse, error) {
		return getVMInfo(ctx, hc)
	}); err != nil {
		return fmt.Errorf("cloud-hypervisor did not answer vm.info after launch (see vm logs): %w", err)
	}
	return nil
}

// queryConsolePTY GETs vm.info for the virtio-console PTY path; "" if console is not in Pty mode.
func queryConsolePTY(ctx context.Context, apiSocketPath string) (string, error) {
	info, err := getVMInfo(ctx, utils.NewSocketHTTPClient(apiSocketPath))
	if err != nil {
		return "", err
	}
	if info.Config.Console.File == "" {
		return "", fmt.Errorf("console PTY not available (mode=%s)", info.Config.Console.Mode)
	}
	return info.Config.Console.File, nil
}

// resolveConsole returns the CH-allocated PTY (direct-boot OCI) or the console socket (UEFI).
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

// saveConsolePTY records the per-boot PTY path for vm inspect; best-effort — the guest is up either way.
func saveConsolePTY(ctx context.Context, vmID, runDir, sockPath string, directBoot bool) {
	if !directBoot {
		return
	}
	pty := resolveConsole(ctx, vmID, sockPath, "", true)
	if pty == "" {
		return
	}
	if err := utils.AtomicWriteFile(hypervisor.ConsolePTYPath(runDir), []byte(pty), 0o600, utils.NoSync); err != nil {
		log.WithFunc("cloudhypervisor.saveConsolePTY").Warnf(ctx, "save console PTY for %s: %v", vmID, err)
	}
}

func qemuExpandImage(ctx context.Context, path string, targetSize int64) error {
	hdr, ok, err := utils.ReadQcow2Header(path)
	if err != nil {
		return fmt.Errorf("read qcow2 header %s: %w", path, err)
	}
	if !ok {
		return fmt.Errorf("%s is not a qcow2 image", path)
	}
	if targetSize <= hdr.VirtualSize {
		return nil
	}
	// shell out: qemu-img is the authoritative qcow2 tool (see utils/qemuimg.go).
	if err := utils.RunQemuImg(ctx, "resize", path, strconv.FormatInt(targetSize, 10)); err != nil {
		return fmt.Errorf("resize %s: %w", path, err)
	}
	return nil
}
