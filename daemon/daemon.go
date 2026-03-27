package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/service"
)

// Daemon is the HTTP API server wrapping a Service instance.
type Daemon struct {
	svc      *service.Service
	server   *http.Server
	listener net.Listener
	addr     string
}

// New creates a Daemon that will serve on the given address.
// addr can be a unix socket path (e.g. "/var/run/cocoon.sock")
// or a TCP address (e.g. "0.0.0.0:9527").
// The listener is created immediately so the address is bound before Start returns.
func New(svc *service.Service, addr string) (*Daemon, error) {
	ln, err := listen(addr)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	d := &Daemon{
		svc:      svc,
		addr:     addr,
		listener: ln,
		server: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}

	d.registerRoutes(mux)

	return d, nil
}

// Start begins serving. Blocks until the context is canceled.
func (d *Daemon) Start(ctx context.Context) error {
	logger := log.WithFunc("daemon.Start")
	logger.Infof(ctx, "daemon listening on %s", d.addr)

	// Shutdown when context is canceled.
	go func() { //nolint:gosec // intentional: request ctx is already canceled, need fresh context for graceful shutdown
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := d.server.Shutdown(shutdownCtx); err != nil {
			logger.Errorf(ctx, err, "graceful shutdown failed")
		}
	}()

	if err := d.server.Serve(d.listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// ListenAddr returns the actual address the listener is bound to.
// Test-only: useful when the original addr used port 0 (auto-assign).
func (d *Daemon) ListenAddr() net.Addr {
	return d.listener.Addr()
}

// listen creates a net.Listener based on the address format.
// Contains ":" → TCP, otherwise → unix socket.
func listen(addr string) (net.Listener, error) {
	if strings.Contains(addr, ":") {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen tcp %s: %w", addr, err)
		}

		return ln, nil
	}

	// Unix socket.
	if err := os.MkdirAll(filepath.Dir(addr), 0o750); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Remove stale socket file.
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", addr, err)
	}

	return ln, nil
}

// --- helpers ---

// refsRequest is a shared request body for endpoints that accept a list of refs.
type refsRequest struct {
	Refs []string `json:"refs"`
}

// writeJSON encodes v as JSON and writes it to the response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck,gosec
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// decodeBody decodes the JSON request body into v.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}

	defer r.Body.Close() //nolint:errcheck

	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}

	return nil
}
