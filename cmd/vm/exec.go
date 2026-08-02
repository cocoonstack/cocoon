package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon-agent/client"
	"github.com/cocoonstack/cocoon/hypervisor"
)

const hybridVsockReplyMax = 256

// ErrVsockNotConfigured is returned for VMs predating vsock support (e.g. restored from a legacy snapshot).
var ErrVsockNotConfigured = errors.New("vsock not configured for this VM")

// ExecExitError carries the agent child's exit code for host-shell propagation.
type ExecExitError struct{ Code int }

func (e *ExecExitError) Error() string { return fmt.Sprintf("exit code %d", e.Code) }

func (h Handler) Exec(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
	ref, argv := args[0], args[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return fmt.Errorf("exec: no command given")
	}

	info, err := h.resolveRunningVM(ctx, conf, "exec", ref)
	if err != nil {
		return err
	}
	if info.VsockSocket == "" {
		return fmt.Errorf("exec: %w (recreate the VM to enable agent exec)", ErrVsockNotConfigured)
	}

	envPairs, _ := cmd.Flags().GetStringArray("env")
	env, err := parseExecEnv(envPairs)
	if err != nil {
		return err
	}

	conn, err := dialHybridVsock(ctx, info.VsockSocket, hypervisor.VsockAgentPort)
	if err != nil {
		return fmt.Errorf("exec: dial agent: %w (cocoon-agent may still be starting; retry shortly)", err)
	}
	defer conn.Close() //nolint:errcheck

	// Stdin is opt-in (docker semantics): a nil reader closes the child's stdin immediately so scripted callers don't have the rest of their script swallowed by the pump.
	var stdin io.Reader
	if interactive, _ := cmd.Flags().GetBool("interactive"); interactive {
		stdin = os.Stdin
	}
	code, err := client.Run(ctx, conn, argv, env, stdin, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if code != 0 {
		// Suppress cobra's "Error: exit code N" so callers (e.g. vk-cocoon) only see the child's own output + exit code.
		cmd.SilenceErrors = true
		return &ExecExitError{Code: code}
	}
	return nil
}

func parseExecEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--env %q must be KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}

// dialHybridVsock dials the UDS + runs CONNECT-port handshake (CH/FC); ctx-aware so Ctrl+C unblocks the "OK " read while the in-guest agent is still coming up.
func dialHybridVsock(ctx context.Context, socketPath string, port uint32) (io.ReadWriteCloser, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, werr := fmt.Fprintf(conn, "CONNECT %d\n", port); werr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", werr)
	}
	reply, err := readHybridVsockReply(conn)
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("read CONNECT reply: %w", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("hybrid vsock CONNECT %d: %s", port, strings.TrimSpace(reply))
	}
	return conn, nil
}

// readHybridVsockReply reads one '\n'-terminated line byte-by-byte; bufio would over-read into the agent's first frame.
func readHybridVsockReply(r io.Reader) (string, error) {
	buf := make([]byte, 0, 32)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if one[0] == '\n' {
				return string(buf), nil
			}
			if len(buf) >= hybridVsockReplyMax {
				return "", fmt.Errorf("reply line exceeds %d bytes", hybridVsockReplyMax)
			}
		}
		if err != nil {
			return "", err
		}
	}
}
