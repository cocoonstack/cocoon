package hypervisor

import (
	"cmp"
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/types"
)

// sysCPURoot is the sysfs cpu tree; tests point it at a fixture.
var sysCPURoot = cgroup.SysCPURoot

// placeRecord resolves id's cpu placement for a launch under cfg and persists it in one write transaction, so concurrent launches see each other's placements.
func (b *Backend) placeRecord(ctx context.Context, id string, cfg *types.Config) (VMRecord, error) {
	var placed VMRecord
	err := b.updateRelaxed(ctx, b.PeerNS, func(t *vmTx) error {
		r, err := t.getRecord(id)
		if err != nil {
			return err
		}
		if err := b.placeVM(ctx, t, r, cfg); err != nil {
			return err
		}
		if err := t.Put(id, r, meta.RelaxedOK); err != nil {
			return err
		}
		placed = *r
		return nil
	})
	return placed, err
}

// placeVM fills rec's placement from cfg: an explicit cpuset is copied; "auto" and no cpuset pick the least-loaded cache domain against the pins live VMs already hold.
func (b *Backend) placeVM(ctx context.Context, t *vmTx, rec *VMRecord, cfg *types.Config) error {
	rec.CPUSet, rec.QueueCPUs = "", ""
	pins := b.PinsQueues && cfg.CPU > 1
	switch {
	case cfg.CPUSetCPUs == "" && !pins:
		return nil
	case cfg.CPUSetCPUs != "" && cfg.CPUSetCPUs != cgroup.AutoCPUSet:
		rec.CPUSet = cfg.CPUSetCPUs
		if pins {
			rec.QueueCPUs = cfg.CPUSetCPUs
		}
		return nil
	}
	fence, err := cgroup.ParseCPUList(b.Conf.CgroupCPUFence())
	if err != nil {
		return err
	}
	topo, err := cgroup.ReadTopology(sysCPURoot, fence)
	if err != nil {
		return fmt.Errorf("read cpu topology: %w", err)
	}
	load, err := b.placementLoad(ctx, t, rec.ID)
	if err != nil {
		return err
	}
	domain, queueCPUs, err := topo.Place(cfg.CPU, load)
	if err != nil {
		return fmt.Errorf("place queue threads: %w", err)
	}
	if pins {
		rec.QueueCPUs = cgroup.FormatCPUList(queueCPUs)
	}
	if cfg.CPUSetCPUs == cgroup.AutoCPUSet {
		rec.CPUSet = cgroup.FormatCPUList(domain)
	}
	return nil
}

// placementLoad counts per host cpu the VMs across every backend holding it: queue pins, or the cpuset of a VM without pins; self is the VM being placed.
func (b *Backend) placementLoad(ctx context.Context, t *vmTx, self string) (map[int]int, error) {
	load := map[int]int{}
	count := func(id string, r *VMRecord) error {
		if id == self {
			return nil
		}
		cpus, _ := cgroup.ParseCPUList(cmp.Or(r.QueueCPUs, r.CPUSet))
		for _, c := range cpus {
			load[c]++
		}
		return nil
	}
	if err := t.Scan(count); err != nil {
		return nil, err
	}
	for _, ns := range b.PeerNS {
		if err := meta.NewCollection[VMRecord](ns, TableRecords).Scan(ctx, t.r, count); err != nil {
			return nil, err
		}
	}
	return load, nil
}
