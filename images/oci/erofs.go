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

// startErofsConversion pipes a tar stream into mkfs.erofs; caller writes+closes stdin, then cmd.Wait().
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

// runErofsConversion streams src into mkfs.erofs at outPath while scanning for boot files under scanDir.
// The scan→drain→close→wait order is load-bearing: mkfs.erofs (and any hasher upstream of src) must
// see the full stream before Wait, and stdin must close before Wait or mkfs.erofs blocks.
func runErofsConversion(ctx context.Context, src io.Reader, scanDir, namePrefix, uuid, outPath string) (kernelPath, initrdPath string, err error) {
	cmd, stdin, output, err := startErofsConversion(ctx, uuid, outPath)
	if err != nil {
		return "", "", fmt.Errorf("start erofs conversion: %w", err)
	}

	tee := io.TeeReader(src, stdin)
	kernelPath, initrdPath, scanErr := scanBootFiles(ctx, tee, scanDir, namePrefix)
	if scanErr == nil {
		if _, drainErr := io.Copy(io.Discard, tee); drainErr != nil {
			scanErr = fmt.Errorf("drain layer stream: %w", drainErr)
		}
	}
	_ = stdin.Close()

	if waitErr := cmd.Wait(); waitErr != nil {
		return "", "", fmt.Errorf("mkfs.erofs failed: %w (output: %s)", waitErr, output.String())
	}
	if scanErr != nil {
		return "", "", fmt.Errorf("scan boot files: %w", scanErr)
	}
	return kernelPath, initrdPath, nil
}
