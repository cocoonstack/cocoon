package cni

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/containernetworking/cni/libcni"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/storage"
	storejson "github.com/cocoonstack/cocoon/storage/json"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const typ = "cni"

var (
	_ network.Network = (*CNI)(nil)

	deleteTAPFn = deleteTAPInNetns // seam for cross-platform teardown tests
)

// CNI implements network.Network using CNI plugins with per-VM netns + bridge + tap.
type CNI struct {
	conf        *Config
	store       storage.Store[networkIndex]
	locker      lock.Locker
	confLists   map[string]*libcni.NetworkConfigList // name → conflist
	defaultName string                               // first conflist name (backward compat)
	cniConf     *libcni.CNIConfig
	loadErr     error // conflist load failure, surfaced by errNoConflist
}

// New creates a CNI provider; conflist loading is best-effort so Delete/Inspect/List still work when none are available — Add fails in that case.
func New(conf *config.Config) (*CNI, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := &Config{Config: conf}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure cni dirs: %w", err)
	}

	locker := flock.New(cfg.IndexLock())
	store := storejson.New[networkIndex](cfg.IndexFile(), locker)

	c := &CNI{
		conf:      cfg,
		store:     store,
		locker:    locker,
		confLists: make(map[string]*libcni.NetworkConfigList),
	}

	if lists, defaultName, loadErr := loadConfLists(cfg.CNIConfDir); loadErr == nil {
		c.confLists = lists
		c.defaultName = defaultName
		c.cniConf = libcni.NewCNIConfigWithCacheDir(
			[]string{cfg.CNIBinDir},
			cfg.CacheDir(),
			nil,
		)
	} else {
		c.loadErr = loadErr
	}

	return c, nil
}

// Type returns the network provider identifier.
func (c *CNI) Type() string { return typ }

// Verify checks whether the network namespace for a VM exists.
func (c *CNI) Verify(_ context.Context, vmID string) error {
	nsPath := netnsPath(vmID)
	if _, err := os.Stat(nsPath); err != nil {
		return fmt.Errorf("netns %s: %w", nsPath, err)
	}
	return nil
}

// Inspect returns the network record for id, or (nil, nil) if not found.
func (c *CNI) Inspect(ctx context.Context, id string) (*types.Network, error) {
	var result *types.Network
	return result, c.store.With(ctx, func(idx *networkIndex) error {
		rec := idx.Networks[id]
		if rec == nil {
			return nil
		}
		net := rec.Network // value copy — detached from the locked index
		result = &net
		return nil
	})
}

// List returns all known network records.
func (c *CNI) List(ctx context.Context) ([]*types.Network, error) {
	var result []*types.Network
	return result, c.store.With(ctx, func(idx *networkIndex) error {
		result = utils.MapValues(idx.Networks, func(rec *networkRecord) *types.Network {
			n := rec.Network
			return &n
		})
		return nil
	})
}

// Delete tears down all NICs for each VM and removes the netns. Best-effort.
func (c *CNI) Delete(ctx context.Context, vmIDs []string) ([]string, error) {
	result := utils.ForEach(ctx, vmIDs, func(ctx context.Context, vmID string) error {
		return c.deleteVM(ctx, vmID)
	})
	return result.Succeeded, result.Err()
}

func (c *CNI) deleteVM(ctx context.Context, vmID string) error {
	var records []networkRecord
	if err := c.store.With(ctx, func(idx *networkIndex) error {
		records = idx.byVMID(vmID)
		return nil
	}); err != nil {
		return fmt.Errorf("read network index: %w", err)
	}
	// Run even when records is empty: a VM resized to 0 NICs still owns its netns.
	nsPath := netnsPath(vmID)
	_, _ = c.tearDownNICs(ctx, vmID, nsPath, records, false)
	nsName := netnsName(vmID)
	if err := deleteNetns(ctx, nsName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove netns %s: %w", nsPath, err)
	}
	allIDs := make([]string, 0, len(records))
	for _, rec := range records {
		allIDs = append(allIDs, rec.ID)
	}
	return c.deleteRecords(ctx, allIDs)
}

// tearDownNICs runs CNI DEL (+ optional TAP delete) on every record, returning the fully-torn-down record IDs (the caller's sweep set) and the joined failures.
func (c *CNI) tearDownNICs(ctx context.Context, vmID, nsPath string, records []networkRecord, deleteTAP bool) ([]string, error) {
	logger := log.WithFunc("cni.tearDownNICs")
	if c.cniConf == nil {
		return nil, c.errNoConflist()
	}
	downIDs := make([]string, 0, len(records))
	var errs []error
	for _, rec := range records {
		var recErr error
		cl, err := c.confListByName(rec.Type)
		if err != nil {
			recErr = err
			logger.Warnf(ctx, "conflist %q not found for CNI DEL %s/%s: %v", rec.Type, vmID, rec.IfName, err)
		}
		if cl != nil {
			if err := c.cniDel(ctx, cl, vmID, nsPath, rec.IfName); err != nil {
				if recErr == nil {
					recErr = fmt.Errorf("cni del %s/%s: %w", vmID, rec.IfName, err)
				}
				logger.Warnf(ctx, "CNI DEL %s/%s: %v", vmID, rec.IfName, err)
			}
		}
		if deleteTAP {
			var idx int
			if _, scanErr := fmt.Sscanf(rec.IfName, "eth%d", &idx); scanErr != nil {
				logger.Warnf(ctx, "parse ifname %q for %s: %v (skip tap delete)", rec.IfName, vmID, scanErr)
			} else if delErr := deleteTAPFn(nsPath, tapNameForVM(vmID, idx)); delErr != nil {
				if recErr == nil {
					recErr = fmt.Errorf("delete tap %s: %w", tapNameForVM(vmID, idx), delErr)
				}
				logger.Warnf(ctx, "delete tap %s in netns %s: %v", tapNameForVM(vmID, idx), nsPath, delErr)
			}
		}
		if recErr != nil {
			errs = append(errs, recErr)
			continue
		}
		downIDs = append(downIDs, rec.ID)
	}
	return downIDs, errors.Join(errs...)
}

func (c *CNI) deleteRecords(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return c.store.Update(ctx, func(idx *networkIndex) error {
		for _, id := range ids {
			delete(idx.Networks, id)
		}
		return nil
	})
}

// confListByName resolves a conflist by name; empty name returns the default (first alphabetically).
func (c *CNI) confListByName(name string) (*libcni.NetworkConfigList, error) {
	if len(c.confLists) == 0 {
		return nil, c.errNoConflist()
	}
	cl, ok := c.confLists[cmp.Or(name, c.defaultName)]
	if !ok {
		return nil, fmt.Errorf("conflist %q not found (available: %s)", name, strings.Join(slices.Sorted(maps.Keys(c.confLists)), ", "))
	}
	return cl, nil
}

func (c *CNI) errNoConflist() error {
	if c.loadErr != nil {
		return fmt.Errorf("%w: load conflists from %s: %w", network.ErrNotConfigured, c.conf.CNIConfDir, c.loadErr)
	}
	return fmt.Errorf("%w: no conflist found in %s", network.ErrNotConfigured, c.conf.CNIConfDir)
}

// loadConfLists loads all .conflist files from dir, returning name→conflist and the default (alphabetically first) name.
func loadConfLists(dir string) (map[string]*libcni.NetworkConfigList, string, error) {
	files, err := libcni.ConfFiles(dir, []string{".conflist"})
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("no .conflist files in %s", dir)
	}
	// files are already sorted by ConfFiles.
	lists := make(map[string]*libcni.NetworkConfigList, len(files))
	var defaultName string
	for _, f := range files {
		cl, parseErr := libcni.ConfListFromFile(f)
		if parseErr != nil {
			return nil, "", fmt.Errorf("parse %s: %w", f, parseErr)
		}
		lists[cl.Name] = cl
		if defaultName == "" {
			defaultName = cl.Name
		}
	}
	return lists, defaultName, nil
}
