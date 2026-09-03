package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func (ch *CloudHypervisor) Console(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	logger := log.WithFunc("cloudhypervisor.Console")
	id, rec, err := ch.ResolveAndLoad(ctx, ref)
	if err != nil {
		return nil, err
	}

	var conn io.ReadWriteCloser
	if err := ch.WithRunningVM(ctx, &rec, func(_ int) error {
		path := resolveConsole(ctx, id, hypervisor.SocketPath(rec.RunDir),
			hypervisor.ConsoleSockPath(rec.RunDir),
			hypervisor.IsDirectBoot(rec.BootConfig))
		if path == "" {
			return fmt.Errorf("no console path for VM %s", id)
		}

		logger.Debugf(ctx, "resolved console path for VM %s: %s", id, path)
		fi, statErr := os.Stat(path)
		if statErr != nil {
			return fmt.Errorf("stat console path %s: %w", path, statErr)
		}

		if fi.Mode()&os.ModeSocket != 0 {
			c, dialErr := (&net.Dialer{}).DialContext(ctx, "unix", path)
			if dialErr != nil {
				return fmt.Errorf("connect to console socket %s: %w", path, dialErr)
			}
			conn = c
		} else {
			f, openErr := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec
			if openErr != nil {
				return fmt.Errorf("open console PTY %s: %w", path, openErr)
			}
			conn = f
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("console %s: %w", id, err)
	}
	return conn, nil
}
