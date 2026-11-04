package cloudhypervisor

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	snapshotTypeDiff = "diff"
	memoryDiffPrefix = "memory-ranges.diff."
)

var _ hypervisor.Migrator = (*CloudHypervisor)(nil)

func (ch *CloudHypervisor) MigrateCapture(ctx context.Context, ref string, req hypervisor.MigrateRequest) (*hypervisor.MigrateRound, error) {
	return ch.MigrateCaptureSequence(ctx, ref, req, ch.migrateSpec(ctx))
}

func (ch *CloudHypervisor) MigrateAbort(ctx context.Context, ref, series string) error {
	return ch.MigrateAbortSequence(ctx, ref, series, ch.migrateSpec(ctx))
}

func (ch *CloudHypervisor) MigrateReceive(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error) {
	var srcCfg *chVMConfig
	spec := hypervisor.CloneSpec{
		VMCfg: vmCfg, Net: net, SnapshotConfig: snapshotConfig,
		AfterExtract: func(ctx context.Context, rec *hypervisor.VMRecord, vmCfg *types.VMConfig, net types.NetSetup, sourceSnapshotID string) (*types.VM, error) {
			return ch.cloneAfterExtractParsed(ctx, rec, vmCfg, net, sourceSnapshotID, srcCfg, true)
		},
	}
	return ch.DirectCloneBase(ctx, vmID, spec, srcDir, func(dstDir, srcDir string) error {
		var err error
		srcCfg, err = cloneSeriesFiles(ctx, dstDir, srcDir)
		return err
	})
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
	}
}

func cloneSeriesFiles(ctx context.Context, dstDir, srcDir string) (*chVMConfig, error) {
	chCfg, err := parseCHConfig(filepath.Join(srcDir, configJSONName))
	if err != nil {
		return nil, fmt.Errorf("parse source config: %w", err)
	}
	classify := classifySnapshotFile(chCfg)
	if err = hypervisor.CloneSnapshotFiles(ctx, dstDir, srcDir, func(name string) hypervisor.SnapshotFileKind {
		if hypervisor.IsMigrateDeltaFile(name) {
			return hypervisor.SnapshotFileSkip
		}
		return classify(name)
	}); err != nil {
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
