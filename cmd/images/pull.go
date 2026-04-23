package images

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
)

func (h Handler) Pull(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	ociStore, cloudimgStore, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool("force")

	for _, image := range args {
		if cmdcore.IsURL(image) {
			if err := h.pullCloudimg(ctx, cloudimgStore, image, force); err != nil {
				return err
			}
		} else {
			if err := h.pullOCI(ctx, ociStore, image); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h Handler) pullOCI(ctx context.Context, store *oci.OCI, image string) error {
	logger := log.WithFunc("cmd.images.pullOCI")
	tracker := progress.NewTracker(func(e ociProgress.Event) {
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
	if err := store.Pull(ctx, image, false, tracker); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

func (h Handler) pullCloudimg(ctx context.Context, store *cloudimg.CloudImg, url string, force bool) error {
	logger := log.WithFunc("cmd.images.pullCloudimg")
	tracker := progress.NewTracker(func(e cloudimgProgress.Event) {
		switch e.Phase {
		case cloudimgProgress.PhaseDownload:
			switch {
			case e.BytesDone == 0 && e.BytesTotal > 0:
				logger.Infof(ctx, "downloading cloud image %s (%s)", url, cmdcore.FormatSize(e.BytesTotal))
			case e.BytesDone == 0:
				logger.Infof(ctx, "downloading cloud image %s", url)
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
			logger.Infof(ctx, "done: %s", url)
		}
	})
	if err := store.Pull(ctx, url, force, tracker); err != nil {
		return fmt.Errorf("pull %s: %w", url, err)
	}
	return nil
}
