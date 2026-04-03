package cni

import (
	"context"
	"fmt"
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

// compile-time interface check.
var _ network.Network = (*CNI)(nil)

const typ = "cni"

// CNI implements network.Network using CNI plugins with per-VM netns + bridge + tap.
type CNI struct {
	conf        *Config
	store       storage.Store[networkIndex]
	locker      lock.Locker
	confLists   map[string]*libcni.NetworkConfigList // name → conflist
	defaultName string                               // first conflist name (backward compat)
	cniConf     *libcni.CNIConfig
}

// New creates a CNI network provider.
// CNI conflist loading is best-effort at creation time; if no conflist is
// available (e.g. no network needed), Delete/Inspect/List still work.
// Config() will fail if the conflist is not loaded.
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
	}

	return c, nil
}

func (c *CNI) Type() string { return typ }

// Verify checks whether the network namespace for a VM exists.
func (c *CNI) Verify(_ context.Context, vmID string) error {
	nsPath := netnsPath(vmID)
	if _, err := os.Stat(nsPath); err != nil {
		return fmt.Errorf("netns %s: %w", nsPath, err)
	}
	return nil
}

// Inspect returns the network record for a single network ID.
// Returns (nil, nil) if not found.
func (c *CNI) Inspect(ctx context.Context, id string) (*types.Network, error) {
	var result *types.Network
	return result, c.store.With(ctx, func(idx *networkIndex) error {
		rec := idx.Networks[id]
		if rec == nil {
			return nil
		}
		net := rec.Network // value copy
		result = &net
		return nil
	})
}

// List returns all known network records.
func (c *CNI) List(ctx context.Context) ([]*types.Network, error) {
	var result []*types.Network
	return result, c.store.With(ctx, func(idx *networkIndex) error {
		for _, rec := range idx.Networks {
			if rec == nil {
				continue
			}
			net := rec.Network
			result = append(result, &net)
		}
		return nil
	})
}

// Delete removes all network resources for the given VM IDs:
//  1. CNI DEL for each NIC (releases IP from IPAM, removes veth pair).
//  2. Remove the named netns (kernel cleans up bridge + tap automatically).
//  3. Remove network records from the DB.
//
// Best-effort: failing to clean one VM does not block others.
// Returns the VM IDs that were fully cleaned.
func (c *CNI) Delete(ctx context.Context, vmIDs []string) ([]string, error) {
	result := utils.ForEach(ctx, vmIDs, func(ctx context.Context, vmID string) error {
		return c.deleteVM(ctx, vmID)
	})
	return result.Succeeded, result.Err()
}

// deleteVM cleans up all network resources for a single VM.
func (c *CNI) deleteVM(ctx context.Context, vmID string) error {
	// Collect value-copies of records for this VM.
	var records []networkRecord
	if err := c.store.With(ctx, func(idx *networkIndex) error {
		records = idx.byVMID(vmID)
		return nil
	}); err != nil {
		return fmt.Errorf("read network index: %w", err)
	}

	// Nothing to clean — VM had no network or was already cleaned.
	if len(records) == 0 {
		return nil
	}

	nsPath := netnsPath(vmID)

	// CNI DEL for each NIC — releases IPs from IPAM and removes veth pairs.
	// Best-effort: log failures but continue. Netns deletion cleans up
	// devices anyway; CNI DEL is primarily for IPAM bookkeeping.
	c.delNICs(ctx, vmID, nsPath, records)

	// Remove the named netns (unmount bind-mount + remove file).
	// deleteNetns retries briefly to handle async fd cleanup after process kill.
	nsName := netnsName(vmID)
	if err := deleteNetns(ctx, nsName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove netns %s: %w", nsPath, err)
	}

	// Remove records from DB.
	return c.store.Update(ctx, func(idx *networkIndex) error {
		for id, rec := range idx.Networks {
			if rec != nil && rec.VMID == vmID {
				delete(idx.Networks, id)
			}
		}
		return nil
	})
}

// delNICs performs best-effort CNI DEL for each NIC record.
// Failures are logged but never returned — netns deletion cleans up
// devices anyway; CNI DEL is primarily for IPAM bookkeeping.
func (c *CNI) delNICs(ctx context.Context, vmID, nsPath string, records []networkRecord) {
	if c.cniConf == nil {
		return
	}
	logger := log.WithFunc("cni.delNICs")
	for _, rec := range records {
		cl, err := c.confListByName(rec.Type)
		if err != nil {
			logger.Warnf(ctx, "conflist %q not found for CNI DEL %s/%s: %v", rec.Type, vmID, rec.IfName, err)
			continue
		}
		rt := &libcni.RuntimeConf{
			ContainerID: vmID,
			NetNS:       nsPath,
			IfName:      rec.IfName,
		}
		if err := c.cniConf.DelNetworkList(ctx, cl, rt); err != nil {
			logger.Warnf(ctx, "CNI DEL %s/%s: %v", vmID, rec.IfName, err)
		}
	}
}

// confListByName resolves a conflist by name.
// Empty name returns the default (first alphabetically).
func (c *CNI) confListByName(name string) (*libcni.NetworkConfigList, error) {
	if len(c.confLists) == 0 {
		return nil, fmt.Errorf("%w: no conflist found in %s", network.ErrNotConfigured, c.conf.CNIConfDir)
	}
	if name == "" {
		name = c.defaultName
	}
	cl, ok := c.confLists[name]
	if !ok {
		return nil, fmt.Errorf("conflist %q not found (available: %s)", name, strings.Join(slices.Sorted(maps.Keys(c.confLists)), ", "))
	}
	return cl, nil
}

// loadConfLists loads all .conflist files from dir.
// Returns the map of name→conflist and the default name (first file, alphabetically).
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
