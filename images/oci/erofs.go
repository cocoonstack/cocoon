package oci

import (
	"bytes"
	"context"
	"errors"
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

	// erofs-utils < 1.8 tar mode writes corrupt compressed clusters yet exits 0, surfacing only as EUCLEAN at read time (#94).
	erofsMinMajor = 1
	erofsMinMinor = 8
)

var (
	erofsVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

	erofsCheckMu sync.Mutex
	erofsCheckOK bool
)

// runErofsConversion streams src into mkfs.erofs while scanning boot files; the scan→drain→close→wait order is load-bearing (full stream before Wait, stdin closed or mkfs.erofs blocks).
func runErofsConversion(ctx context.Context, src io.Reader, scanDir, namePrefix, uuid, outPath string) (kernelPath, initrdPath string, err error) {
	if err = checkErofsVersion(ctx); err != nil {
		return "", "", err
	}
	// shell out because no Go EROFS writer library; mkfs.erofs is authoritative.
	cmd := exec.CommandContext( //nolint:gosec
		ctx, "mkfs.erofs",
		"--tar=f",
		fmt.Sprintf("-z%s", erofsCompression),
		fmt.Sprintf("-C%d", erofsBlockSize),
		"-T0",
		"-U", uuid,
		outPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", "", fmt.Errorf("create stdin pipe: %w", err)
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Start(); err != nil {
		return "", "", fmt.Errorf("start mkfs.erofs: %w", err)
	}

	tee := io.TeeReader(src, stdin)
	kernelPath, initrdPath, scanErr := scanBootFiles(ctx, tee, scanDir, namePrefix)
	if scanErr == nil {
		if _, drainErr := io.Copy(io.Discard, tee); drainErr != nil {
			scanErr = fmt.Errorf("drain layer stream: %w", drainErr)
		}
	}
	_ = stdin.Close()

	// Join scanErr: a scan abort truncates mkfs.erofs' stdin, so waitErr alone would mask the real cause (e.g. an oversized kernel).
	if waitErr := cmd.Wait(); waitErr != nil {
		return "", "", errors.Join(fmt.Errorf("mkfs.erofs failed: %w (output: %s)", waitErr, output.String()), scanErr)
	}
	if scanErr != nil {
		return "", "", fmt.Errorf("scan boot files: %w", scanErr)
	}
	return kernelPath, initrdPath, nil
}

// checkErofsVersion refuses conversion when mkfs.erofs predates the floor; only success is cached so transient probe failures re-probe on the next conversion.
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

// erofsVersionAtLeast parses the leading "X.Y" from `mkfs.erofs --version` output and compares it to the floor.
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
