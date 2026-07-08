package oci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	erofsBlockSize   = 4096
	erofsCompression = "lz4hc"

	// erofs-utils 1.7.x tar mode writes corrupt compressed clusters for some
	// inputs and still exits 0; the corruption surfaces only as EUCLEAN at
	// read time (#94). 1.8 is the first safe series.
	erofsMinMajor = 1
	erofsMinMinor = 8
)

var (
	erofsVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

	erofsCheckMu sync.Mutex
	erofsCheckOK bool
)

// startErofsConversion pipes a tar stream into mkfs.erofs; caller writes+closes stdin, then cmd.Wait().
func startErofsConversion(ctx context.Context, uuid, outputPath string) (cmd *exec.Cmd, stdin io.WriteCloser, output *bytes.Buffer, err error) {
	if err = checkErofsVersion(ctx); err != nil {
		return nil, nil, nil, err
	}
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

// checkErofsVersion refuses conversion when mkfs.erofs predates the floor.
// Only success is cached: a transient probe failure or an in-place
// erofs-utils upgrade is re-probed on the next conversion.
func checkErofsVersion(ctx context.Context) error {
	erofsCheckMu.Lock()
	defer erofsCheckMu.Unlock()
	if erofsCheckOK {
		return nil
	}
	out, err := exec.CommandContext(ctx, "mkfs.erofs", "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("probe mkfs.erofs version: %w (output: %s)", err, bytes.TrimSpace(out))
	}
	if err := erofsVersionAtLeast(string(out)); err != nil {
		return err
	}
	erofsCheckOK = true
	return nil
}

// erofsVersionAtLeast parses the leading "X.Y" from `mkfs.erofs --version`
// output ("mkfs.erofs (erofs-utils) 1.8.10") and compares it to the floor.
func erofsVersionAtLeast(output string) error {
	m := erofsVersionRe.FindStringSubmatch(output)
	if m == nil {
		return fmt.Errorf("cannot parse mkfs.erofs version from %q", strings.TrimSpace(output))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > erofsMinMajor || (major == erofsMinMajor && minor >= erofsMinMinor) {
		return nil
	}
	return fmt.Errorf("mkfs.erofs %s.%s is older than %d.%d: its tar mode silently corrupts layers (EUCLEAN at read time) — upgrade erofs-utils",
		m[1], m[2], erofsMinMajor, erofsMinMinor)
}
