package hypervisor

import (
	"context"
	"fmt"
	"slices"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/types"
)

// sysCPURoot is the sysfs cpu tree; tests point it at a fixture.
var sysCPURoot = cgroup.SysCPURoot

// placeRecord resolves and persists id's cpu placement in one write transaction so concurrent launches see each other.
func (b *Backend) placeRecord(ctx context.Context, id string, cfg *types.Config) (VMRecord, error) {
	topo, topoErr := b.placementTopology(cfg)
	if topoErr != nil {
		return VMRecord{}, topoErr
	}
	var placed VMRecord
	err := b.updateRelaxed(ctx, b.PeerNS, func(t *vmTx) error {
		r, err := t.getRecord(id)
		if err != nil {
			return err
		}
		if err := b.placeVM(ctx, t.r, r, cfg, topo); err != nil {
			return err
		}
		if r.State != types.VMStateCreating && needsQuiesce(r) {
			r.QuiescePending = true
		}
		if err := t.Put(id, r, meta.RelaxedOK); err != nil {
			return err
		}
		placed = *r
		return nil
	})
	return placed, err
}

// placementTopology reads the host cache domains; nil when cfg names cpus or pins nothing.
func (b *Backend) placementTopology(cfg *types.Config) (*cgroup.Topology, error) {
	if cfg.CPUSetCPUs != cgroup.AutoCPUSet && (cfg.CPUSetCPUs != "" || !b.pinsQueues(cfg)) {
		return nil, nil
	}
	b.topoOnce.Do(func() {
		fence, err := cgroup.ParseCPUList(b.Conf.CgroupCPUFence())
		if err != nil {
			b.topoErr = err
			return
		}
		if b.topo, err = cgroup.ReadTopology(sysCPURoot, fence); err != nil {
			b.topoErr = fmt.Errorf("read cpu topology: %w", err)
		}
	})
	return b.topo, b.topoErr
}

// placeVM fills rec's placement from cfg; with a topology it picks the least-loaded cache domain.
func (b *Backend) placeVM(ctx context.Context, r meta.Reader, rec *VMRecord, cfg *types.Config, topo *cgroup.Topology) error {
	rec.CPUSet, rec.QueueCPUs = "", ""
	pins := b.pinsQueues(cfg)
	if topo == nil {
		if cfg.CPUSetCPUs != "" {
			rec.CPUSet = cfg.CPUSetCPUs
			if pins {
				rec.QueueCPUs = cfg.CPUSetCPUs
			}
		}
		return nil
	}
	load, err := b.placementLoad(ctx, r, rec.ID)
	if err != nil {
		return err
	}
	queues := 1
	if pins {
		queues = cfg.CPU
	}
	domain, queueCPUs, err := topo.Place(queues, load)
	if err != nil {
		return err
	}
	if pins {
		rec.QueueCPUs = cgroup.FormatCPUList(queueCPUs)
	}
	if cfg.CPUSetCPUs == cgroup.AutoCPUSet {
		rec.CPUSet = cgroup.FormatCPUList(domain)
	}
	return nil
}

func (b *Backend) pinsQueues(cfg *types.Config) bool {
	return b.PinsQueues && cfg.CPU > 1
}

// placementLoad counts live VMs per host cpu across all backends, excluding self.
func (b *Backend) placementLoad(ctx context.Context, r meta.Reader, self string) (map[int]int, error) {
	load := map[int]int{}
	count := func(id string, cpuList *string) error {
		if id == self {
			return nil
		}
		cpus, _ := cgroup.ParseCPUList(*cpuList)
		for _, c := range cpus {
			load[c]++
		}
		return nil
	}
	for _, ns := range slices.Concat([]string{b.NS}, b.PeerNS) {
		if err := meta.NewCollection[string](ns, TablePlacements).Scan(ctx, r, count); err != nil {
			return nil, err
		}
	}
	return load, nil
}
