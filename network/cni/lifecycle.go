package cni

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const rollbackTimeout = 30 * time.Second

// Prepare creates the per-VM netns; returns "" with no conflist.
func (c *CNI) Prepare(_ context.Context, vmID string, _ *types.VMConfig) (string, error) {
	if c.cniConf == nil {
		return "", nil
	}
	nsName := c.conf.netnsName(vmID)
	nsPath := c.conf.netnsPath(vmID)
	if _, err := ensureNetnsFn(nsName, nsPath); err != nil {
		return "", fmt.Errorf("ensure netns %s: %w", nsName, err)
	}
	return nsPath, nil
}

// Add creates the netns (if absent) and allocates each NIC's CNI plumbing.
func (c *CNI) Add(ctx context.Context, vmID string, vmCfg *types.VMConfig, specs ...network.AddSpec) (configs []*types.NetworkConfig, retErr error) {
	if c.cniConf == nil {
		return nil, c.errNoConflist()
	}
	if len(specs) == 0 {
		return nil, nil
	}
	confList, err := c.confListByName(vmCfg.Network)
	if err != nil {
		return nil, err
	}
	vmCfg.Network = confList.Name
	logger := log.WithFunc("cni.Add")

	nsName := c.conf.netnsName(vmID)
	nsPath := c.conf.netnsPath(vmID)

	if err = c.guardAdd(ctx, vmID); err != nil {
		return nil, err
	}

	var stale map[string]networkRecord
	if err = c.view(ctx, func(t *netTx) error {
		records, byErr := t.byVMID(vmID)
		if byErr != nil {
			return byErr
		}
		stale = make(map[string]networkRecord, len(records))
		for _, r := range records {
			stale[r.IfName] = r
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read network index: %w", err)
	}

	createdNetns, err := ensureNetnsFn(nsName, nsPath)
	if err != nil {
		return nil, fmt.Errorf("ensure netns %s: %w", nsName, err)
	}

	type addedNIC struct {
		index int
		recID string // "" for recovered NICs whose records pre-exist
	}
	added := make([]addedNIC, 0, len(specs))
	var recIDs map[int]string
	defer func() {
		if retErr == nil {
			return
		}
		// Survives caller cancellation, bounded so a hung plugin can't wedge the caller; a failed DEL keeps its intent record so GC can release the lease later.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer rcancel()
		var releasedIDs []string
		// Intents whose ADD never started need no DEL.
		attempted := make(map[string]struct{}, len(added))
		for _, a := range added {
			attempted[a.recID] = struct{}{}
		}
		for _, id := range recIDs {
			if _, ok := attempted[id]; !ok {
				releasedIDs = append(releasedIDs, id)
			}
		}
		kept := false
		for _, a := range added {
			ifn := ifName(a.index)
			if delErr := c.cniDel(rctx, confList, vmID, nsPath, ifn); delErr != nil {
				logger.Warnf(rctx, "rollback CNI DEL %s/%s: %v (record kept for GC)", vmID, ifn, delErr)
				kept = true
				continue
			}
			// setupTCRedirect creates the TAP; it would leak if the netns persists.
			if !createdNetns {
				if delErr := deleteTAPFn(nsPath, tapNameForVM(vmID, a.index)); delErr != nil {
					logger.Warnf(rctx, "rollback tap delete %s: %v (record kept for GC)", tapNameForVM(vmID, a.index), delErr)
					kept = true
					continue
				}
			}
			if a.recID != "" {
				releasedIDs = append(releasedIDs, a.recID)
			}
		}
		if delErr := c.deleteRecords(rctx, releasedIDs); delErr != nil {
			logger.Warnf(rctx, "rollback records: %v", delErr)
		}
		// A kept record needs the netns for GC's retried DEL.
		if createdNetns && !kept {
			_ = deleteNetnsFn(rctx, nsName)
		}
	}()

	type freshNIC struct {
		index int
		recID string
		cfg   *types.NetworkConfig
	}
	recIDs, err = c.stageNICIntents(ctx, confList, vmID, nsPath, specs, stale)
	if err != nil {
		return nil, err
	}

	configs = make([]*types.NetworkConfig, 0, len(specs))
	fresh := make([]freshNIC, 0, len(specs))
	for _, spec := range specs {
		rt := c.nicRuntime(ctx, confList, vmID, nsPath, spec)
		// Registered before ADD so a mid-ADD failure still gets a cleanup DEL.
		added = append(added, addedNIC{index: spec.Index, recID: recIDs[spec.Index]})

		cfg, addErr := c.provisionNIC(ctx, confList, rt, vmID, nsPath, vmCfg, spec)
		if addErr != nil {
			return nil, addErr
		}
		configs = append(configs, cfg)
		if spec.Existing == nil {
			fresh = append(fresh, freshNIC{index: spec.Index, recID: recIDs[spec.Index], cfg: cfg})
		}
	}

	return configs, c.update(ctx, func(t *netTx) error {
		for _, f := range fresh {
			rec, err := t.Get(f.recID)
			if err != nil {
				return err
			}
			if rec == nil { // intent vanished (concurrent sweep): reinsert
				rec = &networkRecord{ID: f.recID, Type: confList.Name, VMID: vmID, IfName: ifName(f.index)}
			}
			if f.cfg.Network != nil {
				rec.Network = *f.cfg.Network
			}
			if err := t.Put(f.recID, rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// Remove tears down NIC plumbing for the given indices; preserves the netns. A failed NIC keeps its DB record so retry / vm rm / GC can still release its CNI resources.
func (c *CNI) Remove(ctx context.Context, vmID string, indices ...int) error {
	if len(indices) == 0 {
		return nil
	}
	var records []networkRecord
	if err := c.view(ctx, func(t *netTx) error {
		var err error
		records, err = t.byVMID(vmID)
		return err
	}); err != nil {
		return fmt.Errorf("read network index: %w", err)
	}
	wanted := make(map[string]bool, len(indices))
	for _, i := range indices {
		wanted[ifName(i)] = true
	}
	// Pick every matching record, not one per ifname: a failed reclaim can leave duplicates, and DEL is idempotent — skipping one would strand a phantom.
	picked := make([]networkRecord, 0, len(indices))
	found := make(map[string]bool, len(indices))
	for _, r := range records {
		if wanted[r.IfName] {
			picked = append(picked, r)
			found[r.IfName] = true
		}
	}
	for _, i := range indices {
		if ifName := ifName(i); !found[ifName] {
			return fmt.Errorf("nic %d (%s): no record", i, ifName)
		}
	}
	// The payload names record IDs, never NIC indices, so a crash mid-teardown recovers exactly these rows and leaves the netns and the other NICs alone.
	subset := make([]string, 0, len(picked))
	for _, r := range picked {
		subset = append(subset, r.ID)
	}
	return c.teardownProtocol(ctx, vmID, subset, true)
}

// guardAdd finishes an interrupted teardown before new NICs are plumbed; a completed deleting roll-forward refuses the Add.
func (c *CNI) guardAdd(ctx context.Context, vmID string) error {
	rolledForward, err := c.recoverTombstone(ctx, vmID)
	if err != nil {
		return fmt.Errorf("recover interrupted teardown for %s: %w", vmID, err)
	}
	if rolledForward {
		return fmt.Errorf("vm %s network teardown was interrupted; recovery completed, retry the operation: %w", vmID, meta.ErrConflict)
	}
	return nil
}

// stageNICIntents reclaims stale slots and lands every fresh NIC's intent record in one write before any plugin ADD: GC gets per-NIC release context at the cost of a single fsync on the claim path.
func (c *CNI) stageNICIntents(ctx context.Context, confList *libcni.NetworkConfigList, vmID, nsPath string, specs []network.AddSpec, stale map[string]networkRecord) (map[int]string, error) {
	recIDs := make(map[int]string, len(specs))
	var intents []*networkRecord
	for _, spec := range specs {
		if spec.Existing != nil {
			continue
		}
		ifName := ifName(spec.Index)
		if rec, ok := stale[ifName]; ok {
			// The index is reusable only after a full reclaim: proceeding would double-allocate on lenient IPAM plugins or bury the root cause on strict ones.
			if rcErr := c.reclaimStaleNIC(ctx, vmID, nsPath, rec); rcErr != nil {
				return nil, fmt.Errorf("reclaim stale NIC %s/%s: %w", vmID, ifName, rcErr)
			}
		}
		recID := utils.GenerateID()
		recIDs[spec.Index] = recID
		intents = append(intents, &networkRecord{ID: recID, Type: confList.Name, VMID: vmID, IfName: ifName})
	}
	if len(intents) == 0 {
		return recIDs, nil
	}
	if err := c.update(ctx, func(t *netTx) error {
		for _, rec := range intents {
			if err := t.Put(rec.ID, rec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("record NIC intents for %s: %w", vmID, err)
	}
	return recIDs, nil
}

func (c *CNI) nicRuntime(ctx context.Context, confList *libcni.NetworkConfigList, vmID, nsPath string, spec network.AddSpec) *libcni.RuntimeConf {
	ifName := ifName(spec.Index)
	rt := &libcni.RuntimeConf{ContainerID: vmID, NetNS: nsPath, IfName: ifName}
	if spec.Existing != nil {
		if delErr := c.cniDel(ctx, confList, vmID, nsPath, ifName); delErr != nil {
			log.WithFunc("cni.nicRuntime").Warnf(ctx, "pre-recovery CNI DEL %s/%s: %v (continuing)", vmID, ifName, delErr)
		}
		if spec.Existing.Network != nil && spec.Existing.Network.IP != "" {
			rt.Args = [][2]string{{"IgnoreUnknown", "1"}, {"IP", spec.Existing.Network.IP}}
		}
	}
	return rt
}

func (c *CNI) provisionNIC(ctx context.Context, confList *libcni.NetworkConfigList, rt *libcni.RuntimeConf, vmID, nsPath string, vmCfg *types.VMConfig, spec network.AddSpec) (*types.NetworkConfig, error) {
	ifName := ifName(spec.Index)
	tapName := tapNameForVM(vmID, spec.Index)

	cniResult, addErr := c.cniConf.AddNetworkList(ctx, confList, rt)
	if addErr != nil {
		return nil, fmt.Errorf("cni add %s/%s: %w", vmID, ifName, addErr)
	}
	netInfo, parseErr := extractNetworkInfo(ctx, cniResult)
	if parseErr != nil {
		return nil, fmt.Errorf("parse CNI result: %w", parseErr)
	}

	var overrideMAC string
	if spec.Existing != nil {
		overrideMAC = spec.Existing.MAC
	}
	queues := network.ResolveQueues(spec.Queues, vmCfg.CPU)
	mac, mtu, setupErr := setupTCRedirectFn(nsPath, ifName, tapName, queues, overrideMAC)
	if setupErr != nil {
		return nil, fmt.Errorf("setup tc-redirect %s: %w", vmID, setupErr)
	}

	var logIP, logGW string
	if netInfo != nil {
		logIP = netInfo.IP
		logGW = netInfo.Gateway
	}
	log.WithFunc("cni.provisionNIC").Debugf(ctx, "NIC %d: %s ip=%s gw=%s tap=%s mac=%s",
		spec.Index, ifName, logIP, logGW, tapName, mac)

	return &types.NetworkConfig{
		TAP:       tapName,
		MAC:       mac,
		NumQueues: queues,
		QueueSize: network.ResolveQueueSize(vmCfg.QueueSize),
		MTU:       mtu,
		Backend:   types.BackendCNI,
		NetnsPath: nsPath,
		Network:   netInfo,
	}, nil
}

func (c *CNI) cniDel(ctx context.Context, confList *libcni.NetworkConfigList, vmID, nsPath, ifName string) error {
	rt := &libcni.RuntimeConf{ContainerID: vmID, NetNS: nsPath, IfName: ifName}
	return c.cniConf.DelNetworkList(ctx, confList, rt)
}

// reclaimStaleNIC releases a record left by a failed detach before its index is reused; any failure keeps the record so the next re-add retries (the teardown is idempotent).
func (c *CNI) reclaimStaleNIC(ctx context.Context, vmID, nsPath string, rec networkRecord) error {
	downIDs, err := c.tearDownNICs(ctx, vmID, nsPath, []networkRecord{rec}, true)
	if err != nil {
		return err
	}
	return c.deleteRecords(ctx, downIDs)
}

func tapNameForVM(vmID string, nic int) string {
	return network.TAPName(tapPrefix, vmID, nic)
}

// ensureNetns creates the netns if missing; bool reports whether this call did the creation.
func ensureNetns(name, nsPath string) (bool, error) {
	if _, err := os.Stat(nsPath); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := createNetns(name); err != nil {
		return false, err
	}
	return true, nil
}

func extractNetworkInfo(ctx context.Context, result cnitypes.Result) (*types.Network, error) {
	newResult, err := current.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("convert CNI result: %w", err)
	}
	if len(newResult.IPs) == 0 {
		return nil, nil
	}

	for _, ipCfg := range newResult.IPs {
		if ipCfg.Address.IP.To4() != nil {
			ones, _ := ipCfg.Address.Mask.Size()
			info := &types.Network{
				IP:     ipCfg.Address.IP.String(),
				Prefix: ones,
			}
			if ipCfg.Gateway != nil {
				info.Gateway = ipCfg.Gateway.String()
			}
			return info, nil
		}
	}
	// IPv6-only plugin results are not persisted; log the drop instead of silently recording nil.
	log.WithFunc("cni.extractNetworkInfo").Warnf(ctx,
		"CNI result has %d IPs but no IPv4; skipping network info (IPv6-only is unsupported)", len(newResult.IPs))
	return nil, nil
}
