package images

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/projecteru2/cocoon/cmd/core"
	"github.com/projecteru2/cocoon/images/cloudimg"
	"github.com/projecteru2/cocoon/progress"
	cloudimgProgress "github.com/projecteru2/cocoon/progress/cloudimg"
	ociProgress "github.com/projecteru2/cocoon/progress/oci"
)

// digestDisplayLen = len("sha256:") + 12 hex digits for compact display.
const digestDisplayLen = 19

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Pull(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.pull")

	for _, image := range args {
		var tracker progress.Tracker

		if cmdcore.IsURL(image) {
			tracker = progress.NewTracker(func(e cloudimgProgress.Event) {
				switch e.Phase {
				case cloudimgProgress.PhaseDownload:
					switch {
					case e.BytesDone == 0 && e.BytesTotal > 0:
						logger.Infof(ctx, "downloading cloud image %s (%s)", image, cmdcore.FormatSize(e.BytesTotal))
					case e.BytesDone == 0:
						logger.Infof(ctx, "downloading cloud image %s", image)
					case e.BytesTotal > 0:
						pct := float64(e.BytesDone) / float64(e.BytesTotal) * 100
						fmt.Printf("\r  %s / %s (%.1f%%)", cmdcore.FormatSize(e.BytesDone), cmdcore.FormatSize(e.BytesTotal), pct)
					default:
						fmt.Printf("\r  %s downloaded", cmdcore.FormatSize(e.BytesDone))
					}
				case cloudimgProgress.PhaseConvert:
					fmt.Println()
					logger.Info(ctx, "converting to qcow2...")
				case cloudimgProgress.PhaseCommit:
					logger.Info(ctx, "committing...")
				case cloudimgProgress.PhaseDone:
					logger.Infof(ctx, "done: %s", image)
				}
			})
		} else {
			tracker = progress.NewTracker(func(e ociProgress.Event) {
				switch e.Phase {
				case ociProgress.PhasePull:
					logger.Infof(ctx, "pulling OCI image %s (%d layers)", image, e.Total)
				case ociProgress.PhaseLayer:
					logger.Infof(ctx, "[%d/%d] %s done", e.Index+1, e.Total, e.Digest)
				case ociProgress.PhaseCommit:
					logger.Info(ctx, "committing...")
				case ociProgress.PhaseDone:
					logger.Infof(ctx, "done: %s", image)
				}
			})
		}

		if err := svc.PullImage(ctx, image, tracker); err != nil {
			return err
		}
	}

	return nil
}

func (h Handler) Import(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	name := args[0]
	files, _ := cmd.Flags().GetStringArray("file")
	logger := log.WithFunc("cmd.import")

	// Build tracker based on file type (OCI progress events vs cloudimg progress events).
	// We peek at the file type here to choose the right typed tracker,
	// since service.ImportImage auto-detects internally.
	var tracker progress.Tracker

	if cloudimg.IsQcow2File(files[0]) {
		tracker = progress.NewTracker(func(e cloudimgProgress.Event) {
			switch e.Phase {
			case cloudimgProgress.PhaseDownload:
				logger.Infof(ctx, "reading %d file(s) for %s", len(files), name)
			case cloudimgProgress.PhaseConvert:
				logger.Info(ctx, "converting to qcow2...")
			case cloudimgProgress.PhaseCommit:
				logger.Info(ctx, "committing...")
			case cloudimgProgress.PhaseDone:
				logger.Infof(ctx, "done: %s", name)
			}
		})
	} else {
		tracker = progress.NewTracker(func(e ociProgress.Event) {
			switch e.Phase {
			case ociProgress.PhasePull:
				logger.Infof(ctx, "importing %s (%d layers)", name, e.Total)
			case ociProgress.PhaseLayer:
				logger.Infof(ctx, "[%d/%d] %s done", e.Index+1, e.Total, e.Digest)
			case ociProgress.PhaseCommit:
				logger.Info(ctx, "committing...")
			case ociProgress.PhaseDone:
				logger.Infof(ctx, "done: %s", name)
			}
		})
	}

	return svc.ImportImage(ctx, name, files, tracker)
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	all, err := svc.ListImages(ctx)
	if err != nil {
		return err
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

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.image.rm")

	deleted, err := svc.RemoveImages(ctx, args)
	for _, ref := range deleted {
		logger.Infof(ctx, "deleted: %s", ref)
	}

	if err != nil {
		return err
	}

	if len(deleted) == 0 {
		logger.Warn(ctx, "no matching images found")
	}

	return nil
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	img, err := svc.InspectImage(ctx, args[0])
	if err != nil {
		return err
	}

	return cmdcore.OutputJSON(img)
}
