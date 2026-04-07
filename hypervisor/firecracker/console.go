package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
)

// Console connects to the FC VM's serial console via the PTY relay socket.
// The relay process (started alongside FC) listens on console.sock and
// bridges connections to the PTY master connected to FC's stdin/stdout.
func (fc *Firecracker) Console(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	id, err := fc.resolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}

	rec, err := fc.loadRecord(ctx, id)
	if err != nil {
		return nil, err
	}

	var conn io.ReadWriteCloser
	if err := fc.withRunningVM(ctx, &rec, func(_ int) error {
		path := consoleSockPath(rec.RunDir)
		c, dialErr := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if dialErr != nil {
			return fmt.Errorf("connect to console socket %s: %w", path, dialErr)
		}
		conn = c
		return nil
	}); err != nil {
		return nil, fmt.Errorf("console %s: %w", id, err)
	}
	return conn, nil
}
