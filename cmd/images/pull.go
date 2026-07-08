package images

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
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
		if cliutil.IsURL(image) {
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
	tracker := ociTracker(ctx, logger, image, func(total int) string {
		return fmt.Sprintf("pulling OCI image %s (%d layers)", image, total)
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
				logger.Infof(ctx, "downloading cloud image %s (%s)", url, cliutil.FormatSize(e.BytesTotal))
			case e.BytesDone == 0:
				logger.Infof(ctx, "downloading cloud image %s", url)
			case e.BytesTotal > 0:
				pct := float64(e.BytesDone) / float64(e.BytesTotal) * 100
				fmt.Printf("\r  %s / %s (%.1f%%)", cliutil.FormatSize(e.BytesDone), cliutil.FormatSize(e.BytesTotal), pct)
			default:
				fmt.Printf("\r  %s downloaded", cliutil.FormatSize(e.BytesDone))
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
