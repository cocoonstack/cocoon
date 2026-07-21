package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/projecteru2/core/log"
)

const (
	apiReadHeaderTimeout = 5 * time.Second
	apiShutdownTimeout   = 2 * time.Second

	// healthStaleFactor bounds how many reconcile intervals may elapse before /healthz reports unready.
	healthStaleFactor = 3

	// DefaultAPISockMode keeps the socket group-accessible; filesystem permissions are the authorization boundary.
	DefaultAPISockMode = 0o660
	apiDirMode         = 0o750
)

// vmsResponse is the /v1/vms body and the stream's opening snapshot.
type vmsResponse struct {
	VMs []VMStatus `json:"vms"`
}

// healthResponse reports whether the loop is running, and how many VMs the last
// pass could not converge. Degraded VMs do not make it unready: restarting the
// daemon does not fix a VM nobody can converge, and a restart loop is worse.
type healthResponse struct {
	OK       bool      `json:"ok"`
	Degraded int       `json:"degraded"`
	LastPass time.Time `json:"last_pass"`
	VMs      int       `json:"vms"`
}

// serveAPI starts the read-only unix-socket API and returns its shutdown hook.
func (d *Daemon) serveAPI(ctx context.Context) (func(), error) {
	ln, err := d.listen()
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: apiReadHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithFunc("daemon.serveAPI").Error(ctx, err, "api server")
		}
	}()
	log.WithFunc("daemon.serveAPI").Infof(ctx, "read-only api on %s", d.conf.APIAddr)
	return func() {
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		_ = os.Remove(d.conf.APIAddr)
	}, nil
}

// listen binds the unix socket, clearing a socket the previous instance left behind — the daemon lock proves no peer owns it.
func (d *Daemon) listen() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(d.conf.APIAddr), apiDirMode); err != nil {
		return nil, err
	}
	if err := os.Remove(d.conf.APIAddr); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", d.conf.APIAddr)
	if err != nil {
		return nil, err
	}
	mode := fs.FileMode(d.conf.APISockMode)
	if mode == 0 {
		mode = DefaultAPISockMode
	}
	if err := os.Chmod(d.conf.APIAddr, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.handleHealth)
	mux.HandleFunc("GET /v1/vms", d.handleVMs)
	mux.HandleFunc("GET /v1/events", d.handleEvents)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	all, h := d.state.snapshot()
	ok := h.ok && !h.lastPass.IsZero() && time.Since(h.lastPass) < healthStaleFactor*d.conf.ReconcileInterval
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, healthResponse{OK: ok, Degraded: h.degraded, LastPass: h.lastPass, VMs: len(all)})
}

func (d *Daemon) handleVMs(w http.ResponseWriter, _ *http.Request) {
	all, _ := d.state.snapshot()
	writeJSON(w, http.StatusOK, vmsResponse{VMs: all})
}

// handleEvents streams changes after an opening snapshot. It replays nothing: a
// reconnecting client compares per-VM generations against the new snapshot to
// find transitions it missed.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, release := d.state.subscribe()
	defer release()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)

	all, _ := d.state.snapshot()
	if err := writeSSE(w, rc, "sync", vmsResponse{VMs: all}); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-events:
			if err := writeSSE(w, rc, "change", ev); err != nil {
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, rc *http.ResponseController, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	return rc.Flush()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
