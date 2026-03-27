package service

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// CreateVM resolves the image, sets up networking, and creates a VM.
func (s *Service) CreateVM(ctx context.Context, p *VMCreateParams) (*types.VM, error) {
	vmCfg := p.toVMConfig()

	if err := vmCfg.Validate(); err != nil {
		return nil, err
	}

	// Resolve image from backends.
	storageConfigs, bootCfg, err := resolveImage(ctx, s.images, vmCfg)
	if err != nil {
		return nil, err
	}

	ensureFirmwarePath(s.conf, bootCfg)

	// Generate VM ID.
	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, fmt.Errorf("generate VM ID: %w", err)
	}

	// Set up network.
	networkConfigs, err := s.setupNetwork(ctx, vmID, p.NICs, vmCfg)
	if err != nil {
		return nil, err
	}

	// Create VM via hypervisor.
	vm, createErr := s.hypervisor.Create(ctx, vmID, vmCfg, storageConfigs, networkConfigs, bootCfg)
	if createErr != nil {
		s.rollbackNetwork(ctx, vmID)
		return nil, fmt.Errorf("create VM: %w", createErr)
	}

	return vm, nil
}

// RunVM creates and starts a VM in one operation.
func (s *Service) RunVM(ctx context.Context, p *VMCreateParams) (*types.VM, error) {
	vm, err := s.CreateVM(ctx, p)
	if err != nil {
		return nil, err
	}

	_, err = s.hypervisor.Start(ctx, []string{vm.ID})
	if err != nil {
		return nil, fmt.Errorf("start VM %s: %w", vm.ID, err)
	}

	return vm, nil
}

// StartVM starts one or more VMs, recovering network if needed.
func (s *Service) StartVM(ctx context.Context, refs []string) ([]string, error) {
	// Pre-start: recover missing netns (e.g. after host reboot).
	if s.network != nil {
		s.recoverNetwork(ctx, refs)
	}

	return s.hypervisor.Start(ctx, refs)
}

// StopVM stops one or more VMs.
func (s *Service) StopVM(ctx context.Context, refs []string) ([]string, error) {
	return s.hypervisor.Stop(ctx, refs)
}

// ListVM returns all VMs sorted by creation time.
// Stale "running" records (process dead) are reconciled to reflect actual state.
func (s *Service) ListVM(ctx context.Context) ([]*types.VM, error) {
	vms, err := s.hypervisor.List(ctx)
	if err != nil {
		return nil, err
	}

	for _, vm := range vms {
		reconcileState(vm)
	}

	slices.SortFunc(vms, func(a, b *types.VM) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	return vms, nil
}

// InspectVM returns a single VM by reference.
// Stale "running" records (process dead) are reconciled to reflect actual state.
func (s *Service) InspectVM(ctx context.Context, ref string) (*types.VM, error) {
	vm, err := s.hypervisor.Inspect(ctx, ref)
	if err != nil {
		return nil, err
	}

	reconcileState(vm)

	return vm, nil
}

// ConsoleVM returns a bidirectional connection to the VM's serial console.
func (s *Service) ConsoleVM(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	return s.hypervisor.Console(ctx, ref)
}

// RemoveVM deletes VMs and cleans up their network resources.
func (s *Service) RemoveVM(ctx context.Context, p *VMRMParams) ([]string, error) {
	deleted, deleteErr := s.hypervisor.Delete(ctx, p.Refs, p.Force)

	// Clean up network for successfully deleted VMs, even on partial error.
	if len(deleted) > 0 && s.network != nil {
		if _, netErr := s.network.Delete(ctx, deleted); netErr != nil {
			return deleted, fmt.Errorf("VM(s) deleted but network cleanup failed: %w", netErr)
		}
	}

	if deleteErr != nil {
		return deleted, fmt.Errorf("rm: %w", deleteErr)
	}

	return deleted, nil
}

// CloneVM clones a new VM from a snapshot.
func (s *Service) CloneVM(ctx context.Context, p *VMCloneParams) (*types.VM, []*types.NetworkConfig, error) {
	snapRef := p.SnapshotRef

	// Try direct fast path.
	if da, ok := s.snapshot.(snapshot.Direct); ok {
		if dcr, ok := s.hypervisor.(hypervisor.Direct); ok {
			return s.cloneDirect(ctx, p, dcr, da, snapRef)
		}
	}

	// Stream fallback.
	cfg, stream, err := s.snapshot.Restore(ctx, snapRef)
	if err != nil {
		return nil, nil, fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	vmCfg, vmID, networkConfigs, err := s.prepareClone(ctx, p, cfg)
	if err != nil {
		return nil, nil, err
	}

	vm, cloneErr := s.hypervisor.Clone(ctx, vmID, vmCfg, networkConfigs, cfg, stream)
	if cloneErr != nil {
		s.rollbackNetwork(ctx, vmID)
		return nil, nil, fmt.Errorf("clone VM: %w", cloneErr)
	}

	return vm, networkConfigs, nil
}

// RestoreVM restores a running VM to a previous snapshot state.
func (s *Service) RestoreVM(ctx context.Context, p *VMRestoreParams) (*types.VM, error) {
	// Verify snapshot belongs to VM.
	vm, err := s.hypervisor.Inspect(ctx, p.VMRef)
	if err != nil {
		return nil, fmt.Errorf("inspect VM: %w", err)
	}

	snapInfo, err := s.snapshot.Inspect(ctx, p.SnapshotRef)
	if err != nil {
		return nil, fmt.Errorf("inspect snapshot: %w", err)
	}

	if _, ok := vm.SnapshotIDs[snapInfo.ID]; !ok {
		return nil, fmt.Errorf("snapshot %s does not belong to VM %s", p.SnapshotRef, p.VMRef)
	}

	// Validate NIC count matches.
	if snapInfo.NICs != len(vm.NetworkConfigs) {
		return nil, fmt.Errorf("NIC count mismatch: VM has %d, snapshot has %d",
			len(vm.NetworkConfigs), snapInfo.NICs)
	}

	// Merge resource params.
	vmCfg, err := mergeRestoreConfig(vm, &snapInfo.SnapshotConfig, p)
	if err != nil {
		return nil, err
	}

	// Try direct fast path.
	if result, ok, directErr := s.restoreDirect(ctx, p, vmCfg); ok {
		return result, directErr
	}

	// Stream fallback.
	_, stream, err := s.snapshot.Restore(ctx, p.SnapshotRef)
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	result, err := s.hypervisor.Restore(ctx, p.VMRef, vmCfg, stream)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}

	return result, nil
}

// DebugInfo holds the data needed to generate a debug CH command.
type DebugInfo struct {
	StorageConfigs []*types.StorageConfig
	BootConfig     *types.BootConfig
	VMConfig       *types.VMConfig
}

// DebugVM resolves image and returns data for building a debug CH command.
func (s *Service) DebugVM(ctx context.Context, p *DebugParams) (*DebugInfo, error) {
	vmCfg := p.toVMConfig()

	storageConfigs, bootCfg, err := resolveImage(ctx, s.images, vmCfg)
	if err != nil {
		return nil, err
	}

	ensureFirmwarePath(s.conf, bootCfg)

	return &DebugInfo{
		StorageConfigs: storageConfigs,
		BootConfig:     bootCfg,
		VMConfig:       vmCfg,
	}, nil
}

// --- internal helpers ---

// reconcileState checks actual process liveness to detect stale "running" records.
func reconcileState(vm *types.VM) {
	if vm.State == types.VMStateRunning && !utils.IsProcessAlive(vm.PID) {
		vm.State = types.VMStateStopped
	}
}

func (p *VMCreateParams) toVMConfig() *types.VMConfig {
	return &types.VMConfig{
		Name:    p.Name,
		CPU:     p.CPU,
		Memory:  p.Memory,
		Storage: p.Storage,
		Image:   p.Image,
		Network: p.Network,
	}
}

func (s *Service) setupNetwork(ctx context.Context, vmID string, nics int, vmCfg *types.VMConfig) ([]*types.NetworkConfig, error) {
	if nics <= 0 || s.network == nil {
		return nil, nil
	}

	configs, err := s.network.Config(ctx, vmID, nics, vmCfg)
	if err != nil {
		return nil, fmt.Errorf("configure network: %w", err)
	}

	return configs, nil
}

func (s *Service) rollbackNetwork(ctx context.Context, vmID string) {
	if s.network == nil {
		return
	}

	if _, err := s.network.Delete(ctx, []string{vmID}); err != nil {
		log.WithFunc("service.rollbackNetwork").Warnf(ctx, "rollback network for %s: %v", vmID, err)
	}
}

func (s *Service) recoverNetwork(ctx context.Context, refs []string) {
	logger := log.WithFunc("service.recoverNetwork")

	for _, ref := range refs {
		vm, err := s.hypervisor.Inspect(ctx, ref)
		if err != nil || vm == nil || len(vm.NetworkConfigs) == 0 {
			continue
		}

		if s.network.Verify(ctx, vm.ID) == nil {
			continue // netns exists, no recovery needed
		}

		logger.Warnf(ctx, "netns missing for VM %s, recovering network", vm.ID)

		if _, recoverErr := s.network.Config(ctx, vm.ID, len(vm.NetworkConfigs), &vm.Config, vm.NetworkConfigs...); recoverErr != nil {
			logger.Warnf(ctx, "recover network for VM %s: %v (start will fail)", vm.ID, recoverErr)
		}
	}
}

func (s *Service) cloneDirect(ctx context.Context, p *VMCloneParams, dcr hypervisor.Direct, da snapshot.Direct, snapRef string) (*types.VM, []*types.NetworkConfig, error) {
	dataDir, cfg, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return nil, nil, fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}

	vmCfg, vmID, networkConfigs, err := s.prepareClone(ctx, p, cfg)
	if err != nil {
		return nil, nil, err
	}

	vm, cloneErr := dcr.DirectClone(ctx, vmID, vmCfg, networkConfigs, cfg, dataDir)
	if cloneErr != nil {
		s.rollbackNetwork(ctx, vmID)
		return nil, nil, fmt.Errorf("clone VM: %w", cloneErr)
	}

	return vm, networkConfigs, nil
}

// restoreDirect attempts the direct restore path. Returns (nil, false, nil) if
// the backends don't support it, so the caller falls through to the stream path.
func (s *Service) restoreDirect(ctx context.Context, p *VMRestoreParams, vmCfg *types.VMConfig) (*types.VM, bool, error) {
	da, ok := s.snapshot.(snapshot.Direct)
	if !ok {
		return nil, false, nil
	}

	dcr, ok := s.hypervisor.(hypervisor.Direct)
	if !ok {
		return nil, false, nil
	}

	dataDir, _, err := da.DataDir(ctx, p.SnapshotRef)
	if err != nil {
		return nil, true, fmt.Errorf("open snapshot: %w", err)
	}

	result, err := dcr.DirectRestore(ctx, p.VMRef, vmCfg, dataDir)
	if err != nil {
		return nil, true, fmt.Errorf("restore: %w", err)
	}

	return result, true, nil
}

func (s *Service) prepareClone(ctx context.Context, p *VMCloneParams, cfg *types.SnapshotConfig) (*types.VMConfig, string, []*types.NetworkConfig, error) {
	vmCfg, err := mergeCloneConfig(cfg, p)
	if err != nil {
		return nil, "", nil, err
	}

	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, "", nil, fmt.Errorf("generate VM ID: %w", err)
	}

	if vmCfg.Name == "" {
		vmCfg.Name = "cocoon-clone-" + vmID[:8]
	}

	nics := p.NICs
	if nics == 0 {
		nics = cfg.NICs
	}

	if nics < cfg.NICs {
		return nil, "", nil, fmt.Errorf("--nics %d below snapshot minimum %d", nics, cfg.NICs)
	}

	networkConfigs, err := s.setupNetwork(ctx, vmID, nics, vmCfg)
	if err != nil {
		return nil, "", nil, err
	}

	return vmCfg, vmID, networkConfigs, nil
}

// mergeCloneConfig builds a VMConfig for clone, inheriting from snapshot
// and validating that resources are >= snapshot minimums.
func mergeCloneConfig(snapCfg *types.SnapshotConfig, p *VMCloneParams) (*types.VMConfig, error) {
	cpu := p.CPU
	if cpu == 0 {
		cpu = snapCfg.CPU
	}

	mem := p.Memory
	if mem == 0 {
		mem = snapCfg.Memory
	}

	stor := p.Storage
	if stor == 0 {
		stor = snapCfg.Storage
	}

	if cpu < snapCfg.CPU {
		return nil, fmt.Errorf("--cpu %d below snapshot minimum %d", cpu, snapCfg.CPU)
	}

	if mem < snapCfg.Memory {
		return nil, fmt.Errorf("--memory below snapshot minimum")
	}

	if stor < snapCfg.Storage {
		return nil, fmt.Errorf("--storage below snapshot minimum")
	}

	cfg := &types.VMConfig{
		Name:    p.Name,
		CPU:     cpu,
		Memory:  mem,
		Storage: stor,
		Image:   snapCfg.Image,
		Network: p.Network,
	}

	return cfg, nil
}

// mergeRestoreConfig builds a VMConfig for restore, keeping current VM values
// by default and validating that resources are >= snapshot minimums.
func mergeRestoreConfig(vm *types.VM, snapCfg *types.SnapshotConfig, p *VMRestoreParams) (*types.VMConfig, error) {
	result := vm.Config // value copy

	if p.CPU > 0 {
		result.CPU = p.CPU
	}

	if p.Memory > 0 {
		result.Memory = p.Memory
	}

	if p.Storage > 0 {
		result.Storage = p.Storage
	}

	if result.CPU < snapCfg.CPU {
		return nil, fmt.Errorf("--cpu %d below snapshot minimum %d", result.CPU, snapCfg.CPU)
	}

	if result.Memory < snapCfg.Memory {
		return nil, fmt.Errorf("--memory below snapshot minimum")
	}

	if result.Storage < snapCfg.Storage {
		return nil, fmt.Errorf("--storage below snapshot minimum")
	}

	return &result, nil
}
