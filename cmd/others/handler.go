package others

import (
	"fmt"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
	"github.com/cocoonstack/cocoon/version"
)

// Handler groups miscellaneous CLI commands (gc, version).
type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) GC(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	policy, err := parseSnapshotPolicy(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}
	netProvider, err := cmdcore.InitNetwork(conf)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf, localfile.WithGCPolicy(policy))
	if err != nil {
		return err
	}

	o := gc.New()
	for _, b := range backends {
		b.RegisterGC(o)
	}
	// Register ALL hypervisor backends so GC protects blobs from both CH and FC VMs.
	hypers, hyperErr := cmdcore.InitAllHypervisors(ctx, conf)
	if hyperErr != nil {
		return hyperErr
	}
	for _, hyper := range hypers {
		hyper.RegisterGC(o)
	}
	netProvider.RegisterGC(o)
	gc.Register(o, bridge.GCModule())
	snapBackend.RegisterGC(o)
	return o.Run(ctx)
}

func (h Handler) Version(_ *cobra.Command, _ []string) error {
	fmt.Print(version.String())
	return nil
}

func parseSnapshotPolicy(cmd *cobra.Command) (localfile.EvictionPolicy, error) {
	enabled, _ := cmd.Flags().GetBool("snapshot")
	keep, _ := cmd.Flags().GetInt("snapshot-keep")
	age, _ := cmd.Flags().GetDuration("snapshot-age")
	sizeStr, _ := cmd.Flags().GetString("snapshot-size")
	dryRun, _ := cmd.Flags().GetBool("snapshot-dry-run")

	if keep < 0 {
		return localfile.EvictionPolicy{}, fmt.Errorf("--snapshot-keep must be >= 0, got %d", keep)
	}
	if age < 0 {
		return localfile.EvictionPolicy{}, fmt.Errorf("--snapshot-age must be >= 0, got %s", age)
	}

	var size int64
	if sizeStr != "" {
		n, err := units.RAMInBytes(sizeStr)
		if err != nil {
			return localfile.EvictionPolicy{}, fmt.Errorf("--snapshot-size %q: %w", sizeStr, err)
		}
		if n < 0 {
			return localfile.EvictionPolicy{}, fmt.Errorf("--snapshot-size must be >= 0, got %s", sizeStr)
		}
		size = n
	}

	if !enabled && (keep > 0 || age > 0 || size > 0 || dryRun) {
		return localfile.EvictionPolicy{}, fmt.Errorf("--snapshot-keep/age/size/dry-run requires --snapshot")
	}

	return localfile.EvictionPolicy{
		Enabled:  enabled,
		DryRun:   dryRun,
		KeepLast: keep,
		MaxAge:   age,
		MaxSize:  size,
	}, nil
}
