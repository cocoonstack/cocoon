package daemon

import (
	"net/http"
	"time"

	"github.com/projecteru2/cocoon/progress"
	cloudimgProgress "github.com/projecteru2/cocoon/progress/cloudimg"
	ociProgress "github.com/projecteru2/cocoon/progress/oci"
	"github.com/projecteru2/cocoon/service"
)

func (d *Daemon) handleListImages(w http.ResponseWriter, r *http.Request) {
	images, err := d.svc.ListImages(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, images)
}

func (d *Daemon) handleInspectImage(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")

	img, err := d.svc.InspectImage(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, img)
}

func (d *Daemon) handleRemoveImages(w http.ResponseWriter, r *http.Request) {
	var req refsRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	deleted, err := d.svc.RemoveImages(r.Context(), req.Refs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// sseProgressEvent is the unified JSON payload for SSE progress events.
type sseProgressEvent struct {
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytes_done,omitempty"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
	Index      int    `json:"index,omitempty"`
	Total      int    `json:"total,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Message    string `json:"message,omitempty"`
}

// handlePullImage pulls an image and streams progress via SSE.
func (d *Daemon) handlePullImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}

	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	sse := newSSEWriter(w, 100*time.Millisecond)
	defer sse.Close()

	// Build typed tracker based on image type.
	var tracker progress.Tracker

	if service.IsURL(req.Ref) {
		tracker = progress.NewTracker(func(e cloudimgProgress.Event) {
			sse.SendProgress(sseProgressEvent{
				Phase:      cloudimgPhaseName(e.Phase),
				BytesDone:  e.BytesDone,
				BytesTotal: e.BytesTotal,
			})
		})
	} else {
		tracker = progress.NewTracker(func(e ociProgress.Event) {
			sse.SendProgress(sseProgressEvent{
				Phase:  ociPhaseName(e.Phase),
				Index:  e.Index,
				Total:  e.Total,
				Digest: e.Digest,
			})
		})
	}

	// Pull blocks until done.
	err := d.svc.PullImage(r.Context(), req.Ref, tracker)

	if err != nil {
		sse.SendEvent("error", sseProgressEvent{Message: err.Error()})
	} else {
		sse.SendEvent("done", sseProgressEvent{Phase: "done"})
	}
}

func cloudimgPhaseName(p cloudimgProgress.Phase) string {
	switch p {
	case cloudimgProgress.PhaseDownload:
		return "download"
	case cloudimgProgress.PhaseConvert:
		return "convert"
	case cloudimgProgress.PhaseCommit:
		return "commit"
	case cloudimgProgress.PhaseDone:
		return "done"
	default:
		return "unknown"
	}
}

func ociPhaseName(p ociProgress.Phase) string {
	switch p {
	case ociProgress.PhasePull:
		return "pull"
	case ociProgress.PhaseLayer:
		return "layer"
	case ociProgress.PhaseCommit:
		return "commit"
	case ociProgress.PhaseDone:
		return "done"
	default:
		return "unknown"
	}
}
