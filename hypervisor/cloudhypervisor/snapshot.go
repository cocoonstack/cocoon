package cloudhypervisor

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	balloonPollInterval  = 100 * time.Millisecond
	balloonStallWindow   = 2 * time.Second
	balloonStallPercent  = 1
	balloonSettleTimeout = time.Minute

	ejectRenotifyInterval = 5 * time.Second
)

func (ch *CloudHypervisor) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, string, error) {
	return ch.SnapshotSequence(ctx, ref, ch.snapshotSpec(ctx))
}

func (ch *CloudHypervisor) Hibernate(ctx context.Context, ref string, persist hypervisor.PersistFunc) error {
	return ch.HibernateSequence(ctx, ref, hypervisor.HibernateSpec{
		SnapshotSpec: ch.snapshotSpec(ctx),
		Terminate: func(rec *hypervisor.VMRecord, pid int) error {
			return ch.forceTerminate(ctx, hypervisor.SocketPath(rec.RunDir), pid)
		},
		RuntimeFiles: runtimeFiles,
	}, persist)
}

func (ch *CloudHypervisor) snapshotSpec(ctx context.Context) hypervisor.SnapshotSpec {
	return hypervisor.SnapshotSpec{
		Pause: func(rec *hypervisor.VMRecord, hc *http.Client) error {
			info, err := getVMInfo(ctx, hc)
			if err != nil {
				return err
			}
			if err := hotAttachedError(info.Config.Fs, info.Config.Devices, info.Config.Disks); err != nil {
				return err
			}
			if len(info.Config.Nets) != len(rec.NetworkConfigs) {
				return fmt.Errorf("vm %s has %d NICs but its record has %d; rerun vm net first", rec.ID, len(info.Config.Nets), len(rec.NetworkConfigs))
			}
			if err := waitPendingNICEjects(ctx, hc, rec, info); err != nil {
				return err
			}
			if err := waitBalloonSettled(ctx, hc, rec.ID, info); err != nil {
				return err
			}
			return pauseVM(ctx, hc)
		},
		Resume: func(_ *hypervisor.VMRecord, hc *http.Client) error { return resumeVM(context.WithoutCancel(ctx), hc) },
		Capture: func(rec *hypervisor.VMRecord, hc *http.Client, tmpDir string) error {
			if err := snapshotVM(ctx, hc, tmpDir, ""); err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			return hypervisor.CopyWritableDisks(ctx, tmpDir, rec)
		},
		AfterCapture: func(rec *hypervisor.VMRecord, tmpDir string) error {
			if hypervisor.IsDirectBoot(rec.BootConfig) || rec.Config.Windows {
				return nil
			}
			cidataSrc := hypervisor.DiskPathByRole(rec.StorageConfigs, types.StorageRoleCidata)
			if cidataSrc == "" { // pre-first-boot: file exists but is not yet a recorded disk
				cidataSrc = filepath.Join(rec.RunDir, cidataFile)
			}
			if !utils.FileExists(cidataSrc) {
				return nil
			}
			if cpErr := utils.SparseCopy(filepath.Join(tmpDir, cidataFile), cidataSrc, utils.NoSync); cpErr != nil {
				return fmt.Errorf("copy cidata: %w", cpErr)
			}
			return nil
		},
		BuildMeta: buildSnapshotMeta,
	}
}

// buildSnapshotMeta mirrors config.json's disk shape (not activeDisks — diverges for cloudimg post-FirstBooted pre-restart where CH still holds cidata).
func buildSnapshotMeta(rec *hypervisor.VMRecord, tmpDir string) (*snapshot.SnapshotMeta, error) {
	chCfg, err := parseCHConfig(filepath.Join(tmpDir, configJSONName))
	if err != nil {
		return nil, fmt.Errorf("parse snapshot config: %w", err)
	}
	byPath := make(map[string]*types.StorageConfig, len(rec.StorageConfigs))
	for _, sc := range rec.StorageConfigs {
		byPath[sc.Path] = sc
	}
	ordered := make([]*types.StorageConfig, 0, len(chCfg.Disks))
	for _, d := range chCfg.Disks {
		sc, ok := byPath[d.Path]
		if !ok {
			return nil, fmt.Errorf("snapshot config has disk %q not present in VM record", d.Path)
		}
		ordered = append(ordered, sc)
	}
	return &snapshot.SnapshotMeta{
		StorageConfigs: hypervisor.CloneStorageConfigs(ordered),
		BootConfig:     rec.BootConfig,
	}, nil
}

func hotAttachedError(fs []chFs, devices []chDevice, disks []chDisk) error {
	if len(fs) > 0 {
		return fmt.Errorf("hot-attached vhost-user-fs %q: %w", fs[0].Tag, hypervisor.ErrHotAttached)
	}
	if len(devices) > 0 {
		return fmt.Errorf("hot-attached device %q: %w", devices[0].Path, hypervisor.ErrHotAttached)
	}
	for _, d := range disks {
		if name := disk.NameFromID(d.ID); name != "" {
			return fmt.Errorf("hot-attached disk %q: %w", name, hypervisor.ErrHotAttached)
		}
	}
	return nil
}

func waitPendingNICEjects(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord, info *chVMInfoResponse) error {
	if len(pendingNICEjects(info)) == 0 {
		return nil
	}
	renotifyAt := time.Now().Add(ejectRenotifyInterval)
	err := utils.WaitFor(ctx, ejectWaitTimeout, 100*time.Millisecond, func() (bool, error) {
		cur, err := getVMInfo(ctx, hc)
		if err != nil {
			return false, err
		}
		if pending := pendingNICEjects(cur); len(pending) > 0 {
			if time.Now().Before(renotifyAt) {
				return false, nil
			}
			renotifyAt = time.Now().Add(ejectRenotifyInterval)
			for _, id := range pending {
				if rmErr := removeDevicePending(ctx, hc, id); rmErr != nil {
					return false, fmt.Errorf("re-request eject of %s: %w", id, rmErr)
				}
			}
			return false, nil
		}
		if !rec.Config.Windows {
			return true, nil
		}
		ctr, err := getVMCounters(ctx, hc)
		if err != nil {
			return false, err
		}
		return !slices.ContainsFunc(cur.Config.Nets, func(n chNet) bool { return ctr[n.ID]["tx_frames"] == 0 }), nil
	})
	if err == nil || ctx.Err() != nil {
		return err
	}
	log.WithFunc("cloudhypervisor.waitPendingNICEjects").Warnf(ctx, "vm %s: capturing with a NIC the guest has not ejected or not started: %v", rec.ID, err)
	return nil
}

func pendingNICEjects(info *chVMInfoResponse) []string {
	var ids []string
	for id := range info.DeviceTree {
		isNIC := strings.HasPrefix(id, "_net") || strings.HasPrefix(id, cocoonNetIDPrefix)
		if isNIC && !slices.ContainsFunc(info.Config.Nets, func(n chNet) bool { return n.ID == id }) {
			ids = append(ids, id)
		}
	}
	return ids
}

func waitBalloonSettled(ctx context.Context, hc *http.Client, vmID string, info *chVMInfoResponse) error {
	balloon := info.Config.Balloon
	if balloon == nil || info.State != chStateRunning {
		return nil
	}
	target := info.Config.Memory.Size - balloon.Size
	step := info.Config.Memory.Size * balloonStallPercent / 100
	start := time.Now()
	progressAt, progressFrom := start, info.MemoryActualSize
	for actual := info.MemoryActualSize; actual > target; {
		if time.Since(progressAt) >= balloonStallWindow || time.Since(start) >= balloonSettleTimeout {
			log.WithFunc("cloudhypervisor.waitBalloonSettled").Warnf(ctx, "snapshot VM %s with balloon at %d of %d bytes", vmID, info.Config.Memory.Size-actual, balloon.Size)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(balloonPollInterval):
		}
		cur, err := getVMInfo(ctx, hc)
		if err != nil {
			return err
		}
		actual = cur.MemoryActualSize
		if progressFrom-actual >= step {
			progressAt, progressFrom = time.Now(), actual
		}
	}
	return nil
}
