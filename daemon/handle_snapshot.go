package daemon

import (
	"net/http"

	"github.com/projecteru2/cocoon/service"
)

func (d *Daemon) handleSaveSnapshot(w http.ResponseWriter, r *http.Request) {
	var p service.SnapshotSaveParams
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	snapID, err := d.svc.SaveSnapshot(r.Context(), &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": snapID})
}

func (d *Daemon) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	vmRef := r.URL.Query().Get("vm")

	snapshots, err := d.svc.ListSnapshots(r.Context(), vmRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, snapshots)
}

func (d *Daemon) handleInspectSnapshot(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")

	snap, err := d.svc.InspectSnapshot(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, snap)
}

func (d *Daemon) handleRemoveSnapshots(w http.ResponseWriter, r *http.Request) {
	var req refsRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	deleted, err := d.svc.RemoveSnapshots(r.Context(), req.Refs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}
