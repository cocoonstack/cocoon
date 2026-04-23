package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func (fc *Firecracker) Console(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	id, err := fc.ResolveRef(ctx, ref)
	if err != nil {
		return nil, err
	}

	rec, err := fc.LoadRecord(ctx, id)
	if err != nil {
		return nil, err
	}

	var conn io.ReadWriteCloser
	if err := fc.WithRunningVM(ctx, &rec, func(_ int) error {
		path := hypervisor.ConsoleSockPath(rec.RunDir)
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
