package cloudhypervisor

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	snapshotTypeDiff = "diff"
	memoryDiffPrefix = "memory-ranges.diff."
)

func (ch *CloudHypervisor) MigrateCapture(ctx context.Context, ref string, req hypervisor.MigrateRequest) (*hypervisor.MigrateRound, error) {
	return ch.MigrateCaptureSequence(ctx, ref, req, ch.migrateSpec(ctx))
}

func (ch *CloudHypervisor) MigrateAbort(ctx context.Context, ref, series string) error {
	return ch.MigrateAbortSequence(ctx, ref, series, ch.migrateSpec(ctx))
}

func (ch *CloudHypervisor) MigrateReceive(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	return ch.directClone(ctx, vmID, vmCfg, net, snapshotConfig, srcDir, true)
}

func (ch *CloudHypervisor) migrateSpec(ctx context.Context) hypervisor.MigrateSpec {
	spec := ch.snapshotSpec(ctx)
	spec.Capture = func(_ *hypervisor.VMRecord, hc *http.Client, dir string) error {
		if err := snapshotVM(ctx, hc, dir, snapshotTypeDiff); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		return nil
	}
	return hypervisor.MigrateSpec{
		SnapshotSpec: spec,
		EndSeries: func(_ *hypervisor.VMRecord, hc *http.Client) error {
			return endSnapshotSeries(context.WithoutCancel(ctx), hc)
		},
		Rounds: countMemoryDiffs,
	}
}

func cloneSeriesFiles(ctx context.Context, dstDir, srcDir string) (*chVMConfig, error) {
	chCfg, err := cloneSnapshotFiles(ctx, dstDir, srcDir)
	if err != nil {
		return nil, err
	}
	rounds, err := countMemoryDiffs(srcDir)
	if err != nil {
		return nil, err
	}
	disks := slices.Collect(maps.Keys(identifyCOWFiles(chCfg)))
	return chCfg, hypervisor.ApplyDiskDeltas(ctx, dstDir, srcDir, disks, rounds)
}

func countMemoryDiffs(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read series dir: %w", err)
	}
	rounds := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), memoryDiffPrefix) {
			rounds++
		}
	}
	return rounds, nil
}
