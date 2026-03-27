package daemon

import (
	"net/http"

	"github.com/projecteru2/cocoon/service"
)

const (
	defaultCPU     = 2
	defaultMemory  = 1 << 30        // 1G
	defaultStorage = 10 * (1 << 30) // 10G
	defaultNICs    = 1
)

func applyCreateDefaults(p *service.VMCreateParams) {
	if p.CPU == 0 {
		p.CPU = defaultCPU
	}
	if p.Memory == 0 {
		p.Memory = defaultMemory
	}
	if p.Storage == 0 {
		p.Storage = defaultStorage
	}
	if p.NICs == 0 {
		p.NICs = defaultNICs
	}
}

func (d *Daemon) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var p service.VMCreateParams
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	applyCreateDefaults(&p)
	vm, err := d.svc.CreateVM(r.Context(), &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusCreated, vm)
}

func (d *Daemon) handleRunVM(w http.ResponseWriter, r *http.Request) {
	var p service.VMCreateParams
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	applyCreateDefaults(&p)
	vm, err := d.svc.RunVM(r.Context(), &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusCreated, vm)
}

func (d *Daemon) handleCloneVM(w http.ResponseWriter, r *http.Request) {
	var p service.VMCloneParams
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	vm, networkConfigs, err := d.svc.CloneVM(r.Context(), &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"vm":              vm,
		"network_configs": networkConfigs,
	})
}

func (d *Daemon) handleStartVM(w http.ResponseWriter, r *http.Request) {
	var req refsRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	started, err := d.svc.StartVM(r.Context(), req.Refs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"started": started})
}

func (d *Daemon) handleStopVM(w http.ResponseWriter, r *http.Request) {
	var req refsRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	stopped, err := d.svc.StopVM(r.Context(), req.Refs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"stopped": stopped})
}

func (d *Daemon) handleListVM(w http.ResponseWriter, r *http.Request) {
	vms, err := d.svc.ListVM(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, vms)
}

func (d *Daemon) handleInspectVM(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")

	vm, err := d.svc.InspectVM(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, vm)
}

func (d *Daemon) handleRemoveVM(w http.ResponseWriter, r *http.Request) {
	var p service.VMRMParams
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	deleted, err := d.svc.RemoveVM(r.Context(), &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (d *Daemon) handleRestoreVM(w http.ResponseWriter, r *http.Request) {
	var p service.VMRestoreParams
	if err := decodeBody(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	vm, err := d.svc.RestoreVM(r.Context(), &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, vm)
}
