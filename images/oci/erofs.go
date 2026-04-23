package oci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

const (
	erofsBlockSize   = 4096
	erofsCompression = "lz4hc"
)

// startErofsConversion starts mkfs.erofs to convert tar stream into EROFS.
// The caller writes the tar stream to the returned WriteCloser and must close it
// when done to signal EOF. Call cmd.Wait() after closing stdin to collect the result.
//
// This mirrors start.sh's per-layer conversion:
//
//	mkfs.erofs --tar=f -zlz4hc -C4096 -T0 -U <uuid> output.erofs
func startErofsConversion(ctx context.Context, uuid, outputPath string) (cmd *exec.Cmd, stdin io.WriteCloser, output *bytes.Buffer, err error) {
	// shell out because no Go EROFS writer library; mkfs.erofs is authoritative.
	cmd = exec.CommandContext(ctx, "mkfs.erofs", //nolint:gosec
		"--tar=f",
		fmt.Sprintf("-z%s", erofsCompression),
		fmt.Sprintf("-C%d", erofsBlockSize),
		"-T0",
		"-U", uuid,
		outputPath,
	)

	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	output = &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output

	if err = cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start mkfs.erofs: %w", err)
	}
	return cmd, stdin, output, nil
}
