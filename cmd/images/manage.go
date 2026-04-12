package images

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	imagebackend "github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/types"
)

// List handles the 'image list' command.
func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	var all []*types.Image
	for _, b := range backends {
		imgs, err := b.List(ctx)
		if err != nil {
			return fmt.Errorf("list %s: %w", b.Type(), err)
		}
		all = append(all, imgs...)
	}
	if len(all) == 0 {
		fmt.Println("No images found.")
		return nil
	}

	return cmdcore.OutputFormatted(cmd, all, func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "TYPE\tNAME\tDIGEST\tSIZE\tCREATED") //nolint:errcheck
		for _, img := range all {
			digest := img.ID
			if len(digest) > digestDisplayLen {
				digest = digest[:digestDisplayLen]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
				img.Type, img.Name, digest,
				cmdcore.FormatSize(img.Size),
				img.CreatedAt.Local().Format(time.DateTime))
		}
	})
}

// RM handles the 'image rm' command.
func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.image.rm")
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	// Resolve each ref to its owning backend BEFORE issuing deletes.
	// Without this, iterating all backends and calling Delete on each
	// with the full ref list would remove every same-named entry from
	// every backend — an OCI image and a cloudimg image with the same
	// name would both be destroyed by a single `image rm <name>`.
	refsByBackend := map[imagebackend.Images][]string{}
	for _, ref := range args {
		owner, resolveErr := cmdcore.ResolveImageOwner(ctx, backends, ref)
		if resolveErr != nil {
			return resolveErr
		}
		refsByBackend[owner] = append(refsByBackend[owner], ref)
	}

	var allDeleted []string
	for backend, refs := range refsByBackend {
		deleted, err := backend.Delete(ctx, refs)
		if err != nil {
			return fmt.Errorf("delete %s: %w", backend.Type(), err)
		}
		allDeleted = append(allDeleted, deleted...)
	}
	for _, ref := range allDeleted {
		logger.Infof(ctx, "deleted: %s", ref)
	}
	if len(allDeleted) == 0 {
		logger.Warn(ctx, "no matching images found")
	}
	return nil
}

// Inspect handles the 'image inspect' command.
func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	// Resolve the owning backend first so cross-backend name collisions
	// surface as ErrAmbiguous instead of being silently hidden behind
	// whichever backend happens to come first in the iteration order.
	ref := args[0]
	owner, err := cmdcore.ResolveImageOwner(ctx, backends, ref)
	if err != nil {
		return err
	}
	img, err := owner.Inspect(ctx, ref)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", owner.Type(), err)
	}
	if img == nil {
		// Narrow TOCTOU: ref was deleted between ResolveImageOwner's
		// probe and this re-probe (concurrent image rm or GC). Fail
		// explicitly instead of dumping "null" as JSON.
		return fmt.Errorf("image %q: disappeared during resolve", ref)
	}
	return cmdcore.OutputJSON(img)
}
