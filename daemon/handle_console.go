package daemon

import (
	"io"
	"net/http"

	"github.com/coder/websocket"
	"github.com/projecteru2/core/log"
)

// handleConsoleVM upgrades to WebSocket and relays bidirectional I/O
// between the client and the VM's serial console.
func (d *Daemon) handleConsoleVM(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")

	// Get the VM console connection first — fail fast before upgrading.
	conn, err := d.svc.ConsoleVM(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			log.WithFunc("daemon.handleConsoleVM").Warnf(r.Context(), "close console conn for %s: %v", ref, closeErr)
		}
	}()

	// Upgrade HTTP → WebSocket.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept already wrote the HTTP error response.
		return
	}
	defer ws.CloseNow() //nolint:errcheck

	// Wrap WebSocket as net.Conn (io.ReadWriter).
	ctx := r.Context()
	netConn := websocket.NetConn(ctx, ws, websocket.MessageBinary)

	// Bidirectional relay: VM ↔ WebSocket client.
	errCh := make(chan error, 2) //nolint:mnd

	go func() {
		_, err := io.Copy(netConn, conn) // VM → client
		errCh <- err
	}()

	go func() {
		_, err := io.Copy(conn, netConn) // client → VM
		errCh <- err
	}()

	// Wait for first goroutine to finish (either direction closed).
	<-errCh

	ws.Close(websocket.StatusNormalClosure, "") //nolint:errcheck,gosec
}
