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

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// List handles the 'vm list' command.
func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	hypers, err := cmdcore.InitAllHypervisors(conf)
	if err != nil {
		return err
	}
	vms, err := cmdcore.ListAllVMs(ctx, hypers)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(vms) == 0 {
		fmt.Println("No VMs found.")
		return nil
	}

	sortVMs(vms)

	return cmdcore.OutputFormatted(cmd, vms, func(w *tabwriter.Writer) {
		printVMTable(w, vms)
	})
}

// Status handles the 'vm status' command.
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

	hypers, hyperErr := cmdcore.InitAllHypervisors(conf)
	if hyperErr != nil {
		return hyperErr
	}

	// Set up file watchers for all backends that support it.
	watchCh := mergeWatchChannels(ctx, hypers)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	format, _ := cmd.Flags().GetString("format")

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

func mergeWatchChannels(ctx context.Context, hypers []hypervisor.Hypervisor) <-chan struct{} {
	var channels []<-chan struct{}
	for _, h := range hypers {
		w, ok := h.(hypervisor.Watchable)
		if !ok {
			continue
		}
		ch, err := utils.WatchFile(ctx, w.WatchPath(), 200*time.Millisecond) //nolint:mnd
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
		fmt.Printf("Every %s — press Ctrl+C to quit (%s)\n\n",
			time.Now().Format(time.TimeOnly), time.Now().Format(time.DateOnly))
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

	prev := map[string]vmSnapshot{}
	runLoop(ctx, watchCh, tick, func() {
		vms := listAndFilter(ctx, hypers, filters)
		curr := make(map[string]vmSnapshot, len(vms))
		for _, vm := range vms {
			curr[vm.ID] = takeSnapshot(vm)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for id, snap := range curr {
			old, existed := prev[id]
			switch {
			case !existed:
				printEventRow(w, "ADDED", snap)
			case old != snap:
				printEventRow(w, "MODIFIED", snap)
			}
		}
		for id, snap := range prev {
			if _, exists := curr[id]; !exists {
				printEventRow(w, "DELETED", snap)
			}
		}
		_ = w.Flush()
		prev = curr
	})
}

type vmEvent struct {
	Event string   `json:"event"`
	VM    types.VM `json:"vm"`
}

func statusEventLoopJSON(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time) {
	enc := json.NewEncoder(os.Stdout)
	type snapshotWithVM struct {
		snap vmSnapshot
		vm   types.VM
	}
	prev := map[string]snapshotWithVM{}
	runLoop(ctx, watchCh, tick, func() {
		vms := listAndFilter(ctx, hypers, filters)
		curr := make(map[string]snapshotWithVM, len(vms))
		for _, vm := range vms {
			vm.State = types.VMState(cmdcore.ReconcileState(vm))
			curr[vm.ID] = snapshotWithVM{snap: takeSnapshot(vm), vm: *vm}
		}

		for id, entry := range curr {
			old, existed := prev[id]
			switch {
			case !existed:
				_ = enc.Encode(vmEvent{Event: "ADDED", VM: entry.vm})
			case old.snap != entry.snap:
				_ = enc.Encode(vmEvent{Event: "MODIFIED", VM: entry.vm})
			}
		}
		for id, entry := range prev {
			if _, exists := curr[id]; !exists {
				_ = enc.Encode(vmEvent{Event: "DELETED", VM: entry.vm})
			}
		}
		prev = curr
	})
}

type vmSnapshot struct {
	id, name, state, ip, image string
	cpu                        int
	memory                     int64
}

func takeSnapshot(vm *types.VM) vmSnapshot {
	return vmSnapshot{
		id:     vm.ID,
		name:   vm.Config.Name,
		state:  cmdcore.ReconcileState(vm),
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

func listAndFilter(ctx context.Context, hypers []hypervisor.Hypervisor, filters []string) []*types.VM {
	vms, err := cmdcore.ListAllVMs(ctx, hypers)
	if err != nil {
		log.WithFunc("status").Warnf(ctx, "list: %v", err)
		return nil
	}
	sortVMs(vms)
	if len(filters) == 0 {
		return vms
	}
	var result []*types.VM
	for _, vm := range vms {
		if matchesFilter(vm, filters) {
			result = append(result, vm)
		}
	}
	return result
}

func matchesFilter(vm *types.VM, filters []string) bool {
	for _, f := range filters {
		if vm.ID == f || vm.Config.Name == f {
			return true
		}
		if len(f) >= 3 && strings.HasPrefix(vm.ID, f) {
			return true
		}
	}
	return false
}

func snapshotAll(vms []*types.VM) []vmSnapshot {
	result := make([]vmSnapshot, len(vms))
	for i, vm := range vms {
		result[i] = takeSnapshot(vm)
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
