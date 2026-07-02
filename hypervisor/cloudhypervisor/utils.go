package cloudhypervisor

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

func (ch *CloudHypervisor) saveCmdline(ctx context.Context, rec *hypervisor.VMRecord, args []string) {
	line := ch.conf.CHBinary + " " + strings.Join(args, " ")
	if err := utils.AtomicWriteFile(filepath.Join(rec.RunDir, cmdlineFileName), []byte(line), 0o600); err != nil {
		log.WithFunc("cloudhypervisor.saveCmdline").Warnf(ctx, "save cmdline: %v", err)
	}
}

// cowPath returns the writable COW disk: raw for direct-boot (OCI), qcow2 overlay for UEFI (cloudimg).
func (ch *CloudHypervisor) cowPath(vmID string, directBoot bool) string {
	if directBoot {
		return ch.conf.COWRawPath(vmID)
	}
	return ch.conf.OverlayPath(vmID)
}

// qemuExpandImage grows a disk to targetSize iff smaller; truncate for raw (directBoot), qemu-img resize for qcow2.
func qemuExpandImage(ctx context.Context, path string, targetSize int64, directBoot bool) error {
	if directBoot {
		return hypervisor.ExpandRawImage(path, targetSize)
	}

	virtualSize, err := readQcow2VirtualSize(path)
	if err != nil {
		return fmt.Errorf("read qcow2 virtual size %s: %w", path, err)
	}
	if targetSize <= virtualSize {
		return nil
	}
	// shell out: qemu-img is the authoritative qcow2 tool (see utils/qemuimg.go).
	if err := utils.RunQemuImg(ctx, "resize", path, strconv.FormatInt(targetSize, 10)); err != nil {
		return fmt.Errorf("resize %s: %w", path, err)
	}
	return nil
}

// readQcow2VirtualSize reads the big-endian uint64 virtual size at qcow2 header offset 24.
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
