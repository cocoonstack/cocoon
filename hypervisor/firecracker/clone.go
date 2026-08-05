package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/lock/vmlock"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const sourcePlaceholderProbeInterval = 2 * time.Millisecond

var errBindSetup = errors.New("bind-mount clone redirect unavailable")

type bindRedirectPlan struct {
	binds  [][2]string
	leases []*vmlock.SharedLease
}

func (p *bindRedirectPlan) files() []*os.File {
	files := make([]*os.File, 0, len(p.leases))
	for _, lease := range p.leases {
		files = append(files, lease.File())
	}
	return files
}

func (p *bindRedirectPlan) close() { closeLeases(p.leases) }

// launchCloneFn starts the FC process over the plan's redirected drive FDs.
type launchCloneFn func([]*os.File) (int, *cloneLeaseControl, error)

// cloneLaunch carries one clone launch's anchoring inputs down the startCloneVM chain; src/dst are the snapshot's recorded drives and the clone's rebuilt ones, index-aligned.
type cloneLaunch struct {
	launch           launchCloneFn
	sockPath, runDir string
	networkConfigs   []*types.NetworkConfig
	src, dst         []*types.StorageConfig
}

func (fc *Firecracker) Clone(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, snapshot io.Reader) (*types.VM, error) {
	return fc.CloneFromStream(ctx, vmID, hypervisor.CloneSpec{VMCfg: vmCfg, Net: net, SnapshotConfig: snapshotConfig, AfterExtract: fc.cloneAfterExtract}, snapshot)
}

func (fc *Firecracker) cloneAfterExtract(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, runDir, logDir string, now time.Time, sourceSnapshotID string) (*types.VM, error) {
	if len(vmCfg.DataDisks) > 0 {
		return nil, fmt.Errorf("--data-disk on clone is Cloud Hypervisor only (Firecracker has no disk hotplug): %w", disk.ErrUnsupportedBackend)
	}
	networkConfigs := net.NetworkConfigs
	logger := log.WithFunc("firecracker.cloneAfterExtract")

	meta, err := fc.conf.LoadAndValidateMeta(runDir)
	if err != nil {
		return nil, fmt.Errorf("load snapshot metadata: %w", err)
	}

	cowPath := fc.conf.COWRawPath(vmID)
	snapshotCOW := filepath.Join(runDir, hypervisor.COWRawFileName)
	if renameErr := os.Rename(snapshotCOW, cowPath); renameErr != nil {
		return nil, fmt.Errorf("move COW to canonical path: %w", renameErr)
	}

	storageConfigs, err := rebuildCloneStorage(meta, cowPath)
	if err != nil {
		return nil, err
	}
	if err := types.ValidateStorageConfigs(storageConfigs); err != nil {
		return nil, fmt.Errorf("validate sidecar: %w", err)
	}
	bootCfg := meta.BootConfig
	if err := EnsureVmlinuxBoot(bootCfg); err != nil {
		return nil, err
	}
	blobIDs := hypervisor.ExtractBlobIDs(storageConfigs, bootCfg)

	if verifyErr := hypervisor.VerifyBaseFiles(storageConfigs, bootCfg); verifyErr != nil {
		return nil, fmt.Errorf("verify base files: %w", verifyErr)
	}
	if err := fc.setBootCmdline(bootCfg, storageConfigs, networkConfigs, vmCfg.Name); err != nil {
		return nil, err
	}

	sockPath := hypervisor.SocketPath(runDir)
	launch := func(leaseFiles []*os.File) (int, *cloneLeaseControl, error) {
		return fc.launchProcessWithLeases(ctx, &hypervisor.VMRecord{
			VM:     types.VM{ID: vmID, Config: *vmCfg},
			RunDir: runDir,
			LogDir: logDir,
		}, sockPath, net.NetnsPath, leaseFiles)
	}
	pid, leaseControl, plan, cloneErr := fc.startCloneVM(ctx, cloneLaunch{
		launch: launch, sockPath: sockPath, runDir: runDir,
		networkConfigs: networkConfigs, src: meta.StorageConfigs, dst: storageConfigs,
	})
	if cloneErr != nil {
		fc.MarkError(ctx, vmID)
		return nil, cloneErr
	}
	defer plan.close()

	info := &types.VM{
		ID: vmID, Hypervisor: typ, State: types.VMStateRunning,
		Config: *vmCfg, StorageConfigs: storageConfigs,
		NetSetup:  net,
		CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	hypervisor.SetRunningSockets(info, runDir)
	if err := fc.FinalizeClone(ctx, vmID, info, bootCfg, blobIDs, sourceSnapshotID); err != nil {
		leaseControl.close()
		fc.AbortLaunch(ctx, pid, sockPath, runDir, runtimeFiles)
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}
	// The record is durable and a delivered commit byte (or relay exit) releases the leases; an unconfirmed release must not kill a committed VM.
	if err := leaseControl.commit(); err != nil {
		logger.Warnf(ctx, "source VM lease release unconfirmed: %v", err)
	}

	logger.Infof(ctx, "VM %s cloned from snapshot", vmID)
	return info, nil
}

// startCloneVM launches FC, drives snapshot/load, then resumes and re-anchors. Clone disks are bind-mounted over source-absolute paths in a private mount namespace, so siblings load in parallel without replacing host paths with symlinks.
func (fc *Firecracker) startCloneVM(ctx context.Context, cl cloneLaunch) (int, *cloneLeaseControl, *bindRedirectPlan, error) {
	pid, leaseControl, plan, err := fc.loadCloneSnapshot(ctx, cl)
	if err != nil {
		return 0, nil, nil, err
	}
	if err := fc.resumeAndReanchorClone(ctx, pid, cl); err != nil {
		leaseControl.close()
		plan.close()
		return 0, nil, nil, err
	}
	return pid, leaseControl, plan, nil
}

func (fc *Firecracker) loadCloneSnapshot(ctx context.Context, cl cloneLaunch) (int, *cloneLeaseControl, *bindRedirectPlan, error) {
	netOverrides := buildNetworkOverrides(cl.networkConfigs)
	vsockPath := hypervisor.VsockSockPath(cl.runDir)

	plan, err := holdBindableRedirects(ctx, fc.Conf.RootDirPath(), fc.Conf.RunDir(), cl.src, cl.dst, func(id string) (bool, error) {
		rec, peekErr := fc.PeekRecord(ctx, id)
		return rec != nil, peekErr
	})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("prepare bind redirects: %w", err)
	}

	var (
		pid          int
		leaseControl *cloneLeaseControl
	)
	if len(plan.binds) == 0 {
		pid, leaseControl, err = cl.launch(nil)
	} else {
		pid, err = launchWithBinds(plan.binds, func() (int, error) {
			var launchErr error
			pid, leaseControl, launchErr = cl.launch(plan.files())
			return pid, launchErr
		})
	}
	if err != nil {
		leaseControl.close()
		plan.close()
		return 0, nil, nil, fmt.Errorf("launch FC: %w", err)
	}
	if err := loadSnapshotFC(ctx, cl.sockPath, cl.runDir, netOverrides, vsockPath); err != nil {
		leaseControl.close()
		fc.AbortLaunch(ctx, pid, cl.sockPath, cl.runDir, runtimeFiles)
		plan.close()
		return 0, nil, nil, fmt.Errorf("snapshot/load: %w", err)
	}
	if err := verifyDriveFDs(pid, plan.binds); err != nil {
		leaseControl.close()
		fc.AbortLaunch(ctx, pid, cl.sockPath, cl.runDir, runtimeFiles)
		plan.close()
		return 0, nil, nil, fmt.Errorf("bind redirect lost during load (source mutated concurrently): %w", err)
	}
	return pid, leaseControl, plan, nil
}

func (fc *Firecracker) resumeAndReanchorClone(ctx context.Context, pid int, cl cloneLaunch) (err error) {
	defer func() {
		if err != nil {
			fc.AbortLaunch(ctx, pid, cl.sockPath, cl.runDir, runtimeFiles)
		}
	}()

	hc := utils.NewSocketHTTPClient(cl.sockPath)
	if err = resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	// Re-anchor redirected drives at the clone's own paths: the loaded vmstate still names the source's, and a future snapshot would embed those dangling paths, breaking its restore.
	for _, i := range redirectedDriveIndices(cl.src, cl.dst) {
		if err = patchDrivePath(ctx, hc, fmt.Sprintf(driveIDFmt, i), cl.dst[i].Path); err != nil {
			return fmt.Errorf("re-anchor drive %d: %w", i, err)
		}
	}
	return nil
}

// rebuildCloneStorage rewrites paths per role (Layer→source, COW→cowPath, Data→runDir); cidata rejected.
func rebuildCloneStorage(meta *hypervisor.SnapshotMeta, cowPath string) ([]*types.StorageConfig, error) {
	runDir := filepath.Dir(cowPath)
	configs := hypervisor.CloneStorageConfigs(meta.StorageConfigs)
	for i, sc := range configs {
		switch sc.Role {
		case types.StorageRoleLayer:
		case types.StorageRoleCOW:
			sc.Path = cowPath
		case types.StorageRoleData:
			sc.Path = filepath.Join(runDir, hypervisor.DataDiskBaseName(sc.Serial))
		case types.StorageRoleCidata:
			return nil, fmt.Errorf("snapshot disk[%d] has cidata role; FC does not support cloudimg", i)
		default:
			return nil, fmt.Errorf("snapshot disk[%d] has unknown role %q", i, sc.Role)
		}
	}
	return configs, nil
}

// redirectedDriveIndices lists drives whose source and clone paths differ — the one source of truth for both bind redirects and the re-anchor loop, which must never diverge.
func redirectedDriveIndices(srcConfigs, dstConfigs []*types.StorageConfig) []int {
	var indices []int
	for i, src := range srcConfigs {
		if i < len(dstConfigs) && src.Path != dstConfigs[i].Path {
			indices = append(indices, i)
		}
	}
	return indices
}

// redirectBinds returns dst-over-src bind pairs; repeated source or destination paths cannot represent distinct drives safely.
func redirectBinds(srcConfigs, dstConfigs []*types.StorageConfig) ([][2]string, error) {
	var binds [][2]string
	seenSources := make(map[string]struct{})
	seenDestinations := make(map[string]struct{})
	for _, i := range redirectedDriveIndices(srcConfigs, dstConfigs) {
		if _, dup := seenSources[srcConfigs[i].Path]; dup {
			return nil, fmt.Errorf("duplicate snapshot drive path %s", srcConfigs[i].Path)
		}
		seenSources[srcConfigs[i].Path] = struct{}{}
		if _, dup := seenDestinations[dstConfigs[i].Path]; dup {
			return nil, fmt.Errorf("duplicate clone drive path %s", dstConfigs[i].Path)
		}
		seenDestinations[dstConfigs[i].Path] = struct{}{}
		binds = append(binds, [2]string{srcConfigs[i].Path, dstConfigs[i].Path})
	}
	return binds, nil
}

// holdBindableRedirects holds shared VM-operation leases for managed sources and creates regular bind mountpoints for dead sources.
func holdBindableRedirects(ctx context.Context, rootDir, runRoot string, srcConfigs, dstConfigs []*types.StorageConfig, sourceRecordExists func(string) (bool, error)) (*bindRedirectPlan, error) {
	binds, err := redirectBinds(srcConfigs, dstConfigs)
	if err != nil {
		return nil, err
	}
	leases, err := holdManagedSourceVMLeases(ctx, rootDir, runRoot, srcConfigs, dstConfigs)
	if err != nil {
		return nil, err
	}
	plan := &bindRedirectPlan{binds: binds, leases: leases}
	fail := func(err error) (*bindRedirectPlan, error) {
		plan.close()
		return nil, err
	}
	for _, bind := range binds {
		if sourceErr := ensureBindableSource(ctx, runRoot, bind[0], sourceRecordExists); sourceErr != nil {
			return fail(sourceErr)
		}
	}
	return plan, nil
}

func ensureBindableSource(ctx context.Context, runRoot, path string, sourceRecordExists func(string) (bool, error)) error {
	ready, err := classifySource(path)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	id, managed := hypervisor.VMIDFromRunPath(runRoot, path)
	if !managed {
		return fmt.Errorf("unusable source %s is outside the managed run root", path)
	}
	lockDir := filepath.Join(runRoot, hypervisor.CloneLocksDirName)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("create clone lock dir: %w", err)
	}
	lock := newCOWPathLock(lockDir, path)
	for {
		locked, lockErr := lock.TryLock(ctx)
		if lockErr != nil {
			return fmt.Errorf("prepare source VM %s: %w", id, lockErr)
		}
		if locked {
			prepareErr := prepareManagedSourceLocked(path, id, sourceRecordExists)
			unlockErr := lock.Unlock(ctx)
			return errors.Join(prepareErr, unlockErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prepare source VM %s: %w", id, ctx.Err())
		case <-time.After(sourcePlaceholderProbeInterval):
			ready, probeErr := classifySource(path)
			if probeErr != nil {
				return probeErr
			}
			if ready {
				return nil
			}
		}
	}
}

func prepareManagedSourceLocked(path, id string, sourceRecordExists func(string) (bool, error)) error {
	ready, err := classifySource(path)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	exists, err := sourceRecordExists(id)
	if err != nil {
		return fmt.Errorf("inspect source VM %s: %w", id, err)
	}
	if exists {
		return fmt.Errorf("source VM %s still exists but drive %s is missing", id, path)
	}
	return createBindPlaceholder(path)
}

// classifySource reports whether the bind source is a ready regular file; absent is the only other non-error state.
func classifySource(path string) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		return true, nil
	case err == nil:
		return false, fmt.Errorf("source %s is not a regular file", path)
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect source %s: %w", path, err)
	}
}

func newCOWPathLock(lockDir, cowPath string) *flock.Lock {
	sum := sha256.Sum256([]byte(cowPath))
	return flock.NewTransient(filepath.Join(lockDir, hex.EncodeToString(sum[:8])+"-"+filepath.Base(cowPath)+".clone.lock"))
}

func createBindPlaceholder(path string) error {
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create bind placeholder dir: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect bind placeholder dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("bind placeholder parent %s is not a directory", dir)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // direct child of the configured managed run root
	if errors.Is(err, fs.ErrExist) {
		fi, statErr := os.Lstat(path)
		if statErr == nil && fi.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("bind placeholder %s changed concurrently", path)
	}
	if err != nil {
		return fmt.Errorf("create bind placeholder: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close bind placeholder: %w", err)
	}
	return nil
}

func managedSourceVMIDs(runRoot string, srcConfigs, dstConfigs []*types.StorageConfig) []string {
	var ids []string
	for _, i := range redirectedDriveIndices(srcConfigs, dstConfigs) {
		if id, ok := hypervisor.VMIDFromRunPath(runRoot, srcConfigs[i].Path); ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func holdManagedSourceVMLeases(ctx context.Context, rootDir, runRoot string, srcConfigs, dstConfigs []*types.StorageConfig) ([]*vmlock.SharedLease, error) {
	var leases []*vmlock.SharedLease
	release := func() { closeLeases(leases) }
	for _, id := range managedSourceVMIDs(runRoot, srcConfigs, dstConfigs) {
		lease, err := vmlock.NewSharedLease(ctx, rootDir, id)
		if err != nil {
			release()
			return nil, fmt.Errorf("lease source VM %s: %w", id, err)
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

func closeLeases(leases []*vmlock.SharedLease) {
	for _, lease := range slices.Backward(leases) {
		_ = lease.Close()
	}
}

func buildNetworkOverrides(networkConfigs []*types.NetworkConfig) []fcNetworkOverride {
	var overrides []fcNetworkOverride
	for i, nc := range networkConfigs {
		overrides = append(overrides, fcNetworkOverride{
			IfaceID:     fmt.Sprintf(ifaceIDFmt, i),
			HostDevName: nc.TAP,
		})
	}
	return overrides
}
