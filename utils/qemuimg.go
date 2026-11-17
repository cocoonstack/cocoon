package utils

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/projecteru2/core/log"
)

// RunQemuImg shells out to qemu-img; non-zero exit returns wrapped combined output.
func RunQemuImg(ctx context.Context, args ...string) error {
	// qemu-img is the reference implementation for qcow2 create, convert, rebase and resize.
	log.WithFunc("utils.RunQemuImg").Debugf(ctx, "exec qemu-img %s", strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("qemu-img %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return nil
}
