package images

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
)

// ociTracker logs OCI pull/import phases; pullMsg renders the PhasePull line.
func ociTracker(ctx context.Context, logger *log.Fields, name string, pullMsg func(total int) string) progress.Tracker {
	return progress.NewTracker(func(e ociProgress.Event) {
		switch e.Phase {
		case ociProgress.PhasePull:
			logger.Info(ctx, pullMsg(e.Total))
		case ociProgress.PhaseLayer:
			logger.Infof(ctx, "[%d/%d] %s done", e.Index+1, e.Total, e.Digest)
		case ociProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case ociProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", name)
		}
	})
}

// cloudimgImportTracker logs cloudimg import phases; downloadMsg renders the PhaseDownload line (imports have no byte progress — pullCloudimg keeps its own progress-bar tracker).
func cloudimgImportTracker(ctx context.Context, logger *log.Fields, name string, downloadMsg func() string) progress.Tracker {
	return progress.NewTracker(func(e cloudimgProgress.Event) {
		switch e.Phase {
		case cloudimgProgress.PhaseDownload:
			logger.Info(ctx, downloadMsg())
		case cloudimgProgress.PhaseConvert:
			logger.Info(ctx, "converting to qcow2...")
		case cloudimgProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case cloudimgProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", name)
		}
	})
}
