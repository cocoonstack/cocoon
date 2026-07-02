package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/moby/term"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const statusWatchDebounce = 200 * time.Millisecond

type vmEvent struct {
	Event string   `json:"event"`
	VM    types.VM `json:"vm"`
}

type vmSnapshot struct {
	id, name, state, ip, image string
	cpu                        int
	memory                     int64
}

type eventEmitter struct {
	begin func()
	emit  func(event string, snap vmSnapshot, vm types.VM)
	end   func()
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	vms, err := cmdcore.ListAllVMs(ctx, hypers)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	sortVMs(vms)
	format, _ := cmd.Flags().GetString("format")
	return renderVMList(vms, format)
}

func (h Handler) Status(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	interval, _ := cmd.Flags().GetInt("interval")
	if interval <= 0 {
		interval = 5 //nolint:mnd
	}
	eventMode, _ := cmd.Flags().GetBool("event")
	watchMode, _ := cmd.Flags().GetBool("watch")
	if eventMode && watchMode {
		return fmt.Errorf("--event and --watch are mutually exclusive")
	}
	format, _ := cmd.Flags().GetString("format")

	hypers, hyperErr := cmdcore.InitAllHypervisors(ctx, conf)
	if hyperErr != nil {
		return hyperErr
	}

	if !eventMode && !watchMode {
		return statusOnce(ctx, hypers, args, format)
	}

	watchCh := mergeWatchChannels(ctx, hypers)
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	if eventMode {
		if format == "json" {
			statusEventLoopJSON(ctx, hypers, args, watchCh, ticker.C)
		} else {
			statusEventLoop(ctx, hypers, args, watchCh, ticker.C)
		}
	} else {
		isTTY := term.IsTerminal(os.Stdout.Fd())
		statusRefreshLoop(ctx, hypers, args, watchCh, ticker.C, isTTY)
	}
	return nil
}

// statusOnce prints one snapshot; propagates ListAllVMs error (loop callers swallow).
func statusOnce(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string, format string) error {
	vms, err := cmdcore.ListAllVMs(ctx, hypers)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	vms = applyFilters(vms, filters)
	sortVMs(vms)
	return renderVMList(vms, format)
}

func renderVMList(vms []*types.VM, format string) error {
	if format == "json" {
		if vms == nil {
			vms = []*types.VM{}
		}
		return cliutil.OutputJSON(vms)
	}
	if len(vms) == 0 {
		fmt.Println("No VMs found.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	printVMTable(w, vms)
	return w.Flush()
}

func mergeWatchChannels(ctx context.Context, hypers []hypervisor.Hypervisor) <-chan struct{} {
	var channels []<-chan struct{}
	for _, h := range hypers {
		w, ok := h.(hypervisor.Watchable)
		if !ok {
			continue
		}
		ch, err := utils.WatchFile(ctx, w.WatchPath(), statusWatchDebounce)
		if err == nil {
			channels = append(channels, ch)
		}
	}
	if len(channels) == 0 {
		return nil
	}
	if len(channels) == 1 {
		return channels[0]
	}
	merged := make(chan struct{}, 1)
	for _, ch := range channels {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					select {
					case merged <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	return merged
}

func runLoop(ctx context.Context, watchCh <-chan struct{}, tick <-chan time.Time, fn func()) {
	fn()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-watchCh:
			if !ok {
				return
			}
			fn()
		case <-tick:
			fn()
		}
	}
}

func statusRefreshLoop(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time, isTTY bool) {
	var prev []vmSnapshot
	runLoop(ctx, watchCh, tick, func() {
		vms := listAndFilter(ctx, hypers, filters)
		curr := snapshotAll(vms)
		if slices.Equal(prev, curr) {
			return
		}
		prev = curr
		if isTTY {
			fmt.Print("\033[H\033[2J") //nolint:errcheck
		}
		now := time.Now()
		fmt.Printf("Every %s — press Ctrl+C to quit (%s)\n\n",
			now.Format(time.TimeOnly), now.Format(time.DateOnly))
		if len(vms) == 0 {
			fmt.Println("No VMs found.")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		printVMTable(w, vms)
		_ = w.Flush()
	})
}

func statusEventLoop(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time) {
	fmt.Println("EVENT\tID\tNAME\tSTATE\tCPU\tMEMORY\tIP\tIMAGE") //nolint:errcheck

	var w *tabwriter.Writer
	statusEventDiffLoop(ctx, hypers, filters, watchCh, tick, eventEmitter{
		begin: func() { w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0) },
		emit:  func(event string, snap vmSnapshot, _ types.VM) { printEventRow(w, event, snap) },
		end:   func() { _ = w.Flush() },
	})
}

func statusEventLoopJSON(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time) {
	enc := json.NewEncoder(os.Stdout)
	statusEventDiffLoop(ctx, hypers, filters, watchCh, tick, eventEmitter{
		emit: func(event string, _ vmSnapshot, vm types.VM) {
			_ = enc.Encode(vmEvent{Event: event, VM: vm})
		},
	})
}

// statusEventDiffLoop snapshots every tick, diffs vs previous, emits ADDED/MODIFIED/DELETED.
func statusEventDiffLoop(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time, emitter eventEmitter) {
	type entry struct {
		snap vmSnapshot
		vm   types.VM
	}
	prev := map[string]entry{}
	runLoop(ctx, watchCh, tick, func() {
		vms := listAndFilter(ctx, hypers, filters)
		curr := make(map[string]entry, len(vms))
		for _, vm := range vms {
			if vm == nil {
				continue
			}
			state := cmdcore.ReconcileState(vm)
			vmCopy := *vm
			vmCopy.State = types.VMState(state)
			curr[vm.ID] = entry{snap: takeSnapshot(vm, state), vm: vmCopy}
		}
		if emitter.begin != nil {
			emitter.begin()
		}
		for id, e := range curr {
			old, existed := prev[id]
			switch {
			case !existed:
				emitter.emit("ADDED", e.snap, e.vm)
			case old.snap != e.snap:
				emitter.emit("MODIFIED", e.snap, e.vm)
			}
		}
		for id, e := range prev {
			if _, exists := curr[id]; !exists {
				emitter.emit("DELETED", e.snap, e.vm)
			}
		}
		if emitter.end != nil {
			emitter.end()
		}
		prev = curr
	})
}

func takeSnapshot(vm *types.VM, state string) vmSnapshot {
	return vmSnapshot{
		id:     vm.ID,
		name:   vm.Config.Name,
		state:  state,
		cpu:    vm.Config.CPU,
		memory: vm.Config.Memory,
		ip:     vmIPs(vm),
		image:  vm.Config.Image,
	}
}

func printEventRow(w *tabwriter.Writer, event string, snap vmSnapshot) {
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", //nolint:errcheck
		event, snap.id, snap.name, snap.state,
		snap.cpu, units.BytesSize(float64(snap.memory)),
		snap.ip, snap.image)
}

// listAndFilter warns on backend errors so polling ticks don't break; one-shot callers use cmdcore.ListAllVMs directly.
func listAndFilter(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string) []*types.VM {
	vms, err := cmdcore.ListAllVMs(ctx, hypers)
	if err != nil {
		log.WithFunc("cmd.vm.listAndFilter").Warnf(ctx, "list: %v", err)
		return nil
	}
	sortVMs(vms)
	return applyFilters(vms, filters)
}

func applyFilters(vms []*types.VM, filters []string) []*types.VM {
	if len(filters) == 0 {
		return vms
	}
	out := make([]*types.VM, 0, len(vms))
	for _, vm := range vms {
		if matchesFilter(vm, filters) {
			out = append(out, vm)
		}
	}
	return out
}

func matchesFilter(vm *types.VM, filters []string) bool {
	return slices.ContainsFunc(filters, func(f string) bool {
		return vm.ID == f || vm.Config.Name == f || (len(f) >= 3 && strings.HasPrefix(vm.ID, f))
	})
}

func snapshotAll(vms []*types.VM) []vmSnapshot {
	result := make([]vmSnapshot, len(vms))
	for i, vm := range vms {
		result[i] = takeSnapshot(vm, cmdcore.ReconcileState(vm))
	}
	return result
}

func sortVMs(vms []*types.VM) {
	slices.SortFunc(vms, func(a, b *types.VM) int { return a.CreatedAt.Compare(b.CreatedAt) })
}

func printVMTable(w *tabwriter.Writer, vms []*types.VM) {
	fmt.Fprintln(w, "ID\tNAME\tSTATE\tCPU\tMEMORY\tSTORAGE\tIP\tIMAGE\tCREATED") //nolint:errcheck
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			vm.ID, vm.Config.Name, cmdcore.ReconcileState(vm),
			vm.Config.CPU, units.BytesSize(float64(vm.Config.Memory)),
			units.BytesSize(float64(vm.Config.Storage)),
			vmIPs(vm), vm.Config.Image,
			vm.CreatedAt.Local().Format(time.DateTime))
	}
}

func vmIPs(vm *types.VM) string {
	var ips []string
	for _, nc := range vm.NetworkConfigs {
		if nc != nil && nc.Network != nil && nc.Network.IP != "" {
			ips = append(ips, nc.Network.IP)
		}
	}
	if len(ips) == 0 {
		return "-"
	}
	return strings.Join(ips, ",")
}
