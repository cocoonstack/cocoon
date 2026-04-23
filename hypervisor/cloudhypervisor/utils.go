package cloudhypervisor

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func (ch *CloudHypervisor) saveCmdline(ctx context.Context, rec *hypervisor.VMRecord, args []string) {
	line := ch.conf.CHBinary + " " + strings.Join(args, " ")
	if err := os.WriteFile(filepath.Join(rec.RunDir, "cmdline"), []byte(line), 0o600); err != nil {
		log.WithFunc("cloudhypervisor.saveCmdline").Warnf(ctx, "save cmdline: %v", err)
	}
}

// cowPath returns the writable COW disk path for a VM.
// Direct-boot (OCI) uses a raw file; UEFI (cloudimg) uses a qcow2 overlay.
func (ch *CloudHypervisor) cowPath(vmID string, directBoot bool) string {
	if directBoot {
		return ch.conf.COWRawPath(vmID)
	}
	return ch.conf.OverlayPath(vmID)
}

// qemuExpandImage expands a disk image to targetSize if its current virtual
// size is smaller. For raw/sparse files (directBoot), os.Truncate is used;
// for qcow2 images, qemu-img resize is used. No-op if already large enough.
func qemuExpandImage(ctx context.Context, path string, targetSize int64, directBoot bool) error {
	if directBoot {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if targetSize <= fi.Size() {
			return nil
		}
		if err := os.Truncate(path, targetSize); err != nil {
			return fmt.Errorf("truncate %s to %d: %w", path, targetSize, err)
		}
		return nil
	}

	virtualSize, err := readQcow2VirtualSize(path)
	if err != nil {
		return fmt.Errorf("read qcow2 virtual size %s: %w", path, err)
	}
	if targetSize <= virtualSize {
		return nil
	}
	// shell out because no mature Go qcow2 writer library; qemu-img is authoritative.
	if out, err := exec.CommandContext(ctx, //nolint:gosec
		"qemu-img", "resize", path, fmt.Sprintf("%d", targetSize),
	).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize %s: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// readQcow2VirtualSize reads the virtual size from a qcow2 file header.
// The qcow2 header stores the virtual size as a big-endian uint64 at offset 24.
func readQcow2VirtualSize(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck
	var hdr [32]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}
	return int64(binary.BigEndian.Uint64(hdr[24:32])), nil //nolint:gosec // qcow2 virtual size fits int64
}
