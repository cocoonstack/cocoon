package daemon

import "net/http"

func (d *Daemon) registerRoutes(mux *http.ServeMux) {
	// VM endpoints
	mux.HandleFunc("POST /api/v1/vms", d.handleCreateVM)
	mux.HandleFunc("POST /api/v1/vms/run", d.handleRunVM)
	mux.HandleFunc("POST /api/v1/vms/clone", d.handleCloneVM)
	mux.HandleFunc("POST /api/v1/vms/start", d.handleStartVM)
	mux.HandleFunc("POST /api/v1/vms/stop", d.handleStopVM)
	mux.HandleFunc("GET /api/v1/vms", d.handleListVM)
	mux.HandleFunc("GET /api/v1/vms/{ref}", d.handleInspectVM)
	mux.HandleFunc("DELETE /api/v1/vms", d.handleRemoveVM)
	mux.HandleFunc("POST /api/v1/vms/restore", d.handleRestoreVM)
	mux.HandleFunc("GET /api/v1/vms/{ref}/console", d.handleConsoleVM) // WebSocket

	// Image endpoints
	mux.HandleFunc("GET /api/v1/images", d.handleListImages)
	mux.HandleFunc("GET /api/v1/images/{ref}", d.handleInspectImage)
	mux.HandleFunc("DELETE /api/v1/images", d.handleRemoveImages)
	mux.HandleFunc("POST /api/v1/images/pull", d.handlePullImage)

	// Snapshot endpoints
	mux.HandleFunc("POST /api/v1/snapshots/save", d.handleSaveSnapshot)
	mux.HandleFunc("GET /api/v1/snapshots", d.handleListSnapshots)
	mux.HandleFunc("GET /api/v1/snapshots/{ref}", d.handleInspectSnapshot)
	mux.HandleFunc("DELETE /api/v1/snapshots", d.handleRemoveSnapshots)

	// System endpoints
	mux.HandleFunc("POST /api/v1/gc", d.handleGC)
}
