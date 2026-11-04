package utils

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RunQemuImg shells out to qemu-img; non-zero exit returns wrapped combined output.
func RunQemuImg(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("qemu-img %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return nil
}
