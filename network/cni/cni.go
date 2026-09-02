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
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

const typ = "cni"

// Seams for cross-platform lifecycle tests (netns/TAP ops are linux-only).
var (
	deleteTAPFn       = deleteTAPInNetns
	deleteNetnsFn     = deleteNetns
	ensureNetnsFn     = ensureNetns
	setupTCRedirectFn = setupTCRedirect
	tapPresentFn      = tapPresentInNetns
	statNetnsFn       = os.Stat
	setLinkStateFn    = setLinkStateInNetns
)

var _ network.Network = (*CNI)(nil)

// CNI implements network.Network using CNI plugins with per-VM netns + bridge + tap.
type CNI struct {
	conf        *Config
	meta        meta.Store
	confLists   map[string]*libcni.NetworkConfigList
	defaultName string // first conflist name (backward compat)
	cniConf     *libcni.CNIConfig
	loadErr     error
}

// New creates a CNI provider; conflist loading is best-effort so Delete still works when none is available — Add fails in that case.
func New(conf *config.Config, store meta.Store) (*CNI, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure cni dirs: %w", err)
	}

	c := &CNI{
		conf:      cfg,
		meta:      store,
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

// Verify checks the netns and every expected TAP inside it.
func (c *CNI) Verify(_ context.Context, vmID string, expected []*types.NetworkConfig) error {
	nsPath := c.conf.netnsPath(vmID)
	if _, err := statNetnsFn(nsPath); err != nil {
		return fmt.Errorf("netns %s: %w", nsPath, err)
	}
	for _, nc := range expected {
		if nc == nil || nc.TAP == "" {
			continue
		}
		if err := tapPresentFn(nsPath, nc.TAP); err != nil {
			return err
		}
	}
	return nil
}

// Quiesce brings the VM's host-side veths down so a stopped VM's TC redirect stops storming softirqs against its now-carrier-less TAP (mirred-to-down-device). The netns and TAP are kept for a fast restart, which Unquiesce re-enables.
func (c *CNI) Quiesce(ctx context.Context, vmID string) error {
	return c.setLinkState(ctx, vmID, false)
}

// Unquiesce restores the veths Quiesce brought down, run on start before the VMM re-opens the TAP.
func (c *CNI) Unquiesce(ctx context.Context, vmID string) error {
	return c.setLinkState(ctx, vmID, true)
}

// Delete tears down the VM's networking; the caller already holds the VM lock, so this never re-locks the non-reentrant flock.
func (c *CNI) Delete(ctx context.Context, vmID string) error {
	return c.teardownProtocol(ctx, vmID, nil, false)
}

// tearDownNICs runs CNI DEL (+ optional TAP delete) on every record, returning the fully-torn-down record IDs (the caller's sweep set) and the joined failures.
func (c *CNI) tearDownNICs(ctx context.Context, vmID, nsPath string, records []networkRecord, deleteTAP bool) ([]string, error) {
	logger := log.WithFunc("cni.tearDownNICs")
	// Zero records need no plugin: a missing conflist must not wedge a 0-NIC VM's netns removal.
	if len(records) == 0 {
		return nil, nil
	}
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

func (c *CNI) setLinkState(ctx context.Context, vmID string, up bool) error {
	var records []networkRecord
	if err := c.view(ctx, func(t *netTx) error {
		var err error
		records, err = t.byVMID(vmID)
		return err
	}); err != nil {
		return fmt.Errorf("read network index: %w", err)
	}
	if len(records) == 0 {
		return nil
	}
	ifNames := make([]string, 0, len(records))
	for _, rec := range records {
		ifNames = append(ifNames, rec.IfName)
	}
	nsPath := c.conf.netnsPath(vmID)
	// A host reboot wipes netns but not records; with no netns there is nothing left to quiesce, and failing would retry forever.
	if _, err := statNetnsFn(nsPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return setLinkStateFn(nsPath, ifNames, up)
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
		defaultName = cmp.Or(defaultName, cl.Name)
	}
	return lists, defaultName, nil
}
