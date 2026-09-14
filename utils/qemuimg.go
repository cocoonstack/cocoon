package utils

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RunQemuImg shells out to qemu-img; non-zero exit returns wrapped combined output. Use exec.CommandContext directly when stdout matters.
func RunQemuImg(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("qemu-img: no args")
	}
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("qemu-img %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return nil
}
