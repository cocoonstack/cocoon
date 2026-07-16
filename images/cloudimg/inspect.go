package cloudimg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/utils"
)

// nonImageSignatures catches common payloads qemu-img would misclassify as raw.
var nonImageSignatures = []struct {
	prefix []byte
	desc   string
}{
	{[]byte("<!"), "content looks like HTML/XML, not a disk image"},
	{[]byte("<?"), "content looks like HTML/XML, not a disk image"},
	{[]byte("<h"), "content looks like HTML, not a disk image"},
	{[]byte("<H"), "content looks like HTML, not a disk image"},
	{utils.GzipMagic, "content is gzip-compressed (cloudimg does not auto-decompress)"},
	{[]byte("\xfd7zXZ\x00"), "content is xz-compressed (cloudimg does not auto-decompress)"},
	{[]byte("BZh"), "content is bzip2-compressed (cloudimg does not auto-decompress)"},
	{utils.ZstdMagic, "content is zstd-compressed (cloudimg does not auto-decompress)"},
	{[]byte("PK"), "content is a zip archive, not a disk image"},
	{[]byte{0x37, 0x7a, 0xbc, 0xaf, 0x27, 0x1c}, "content is a 7z archive, not a disk image"},
}

type sourceImageInfo struct {
	Format         string // "qcow2" or "raw"
	Compat         string // qcow2 compat level (e.g. "0.10", "1.1"); empty for non-qcow2
	HasBackingFile bool
}

func sniffImageSource(f *os.File) error {
	head, err := utils.FileHead(f, 8) //nolint:mnd
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	return sniffHead(head)
}

func sniffHead(head []byte) error {
	if len(head) < 4 {
		return fmt.Errorf("content too small to be a disk image (%d bytes)", len(head))
	}
	if bytes.HasPrefix(head, utils.Qcow2Magic) {
		return nil
	}
	for _, sig := range nonImageSignatures {
		if bytes.HasPrefix(head, sig.prefix) {
			return errors.New(sig.desc)
		}
	}
	return nil
}

func inspectImage(ctx context.Context, path string) (*sourceImageInfo, error) {
	info, isQcow2, err := inspectQcow2Header(path)
	if err != nil {
		return nil, err
	}
	if isQcow2 {
		return info, nil
	}
	// Not qcow2: shell out to qemu-img to distinguish raw from unsupported (vmdk/vdi/vhd/etc.) — treating non-qcow2 as raw would corrupt convertToQcow2 output.
	log.WithFunc("cloudimg.inspectImage").Debugf(ctx, "exec qemu-img info %s", path)
	cmd := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path) //nolint:gosec // path is controlled
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	var raw struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse qemu-img info: %w", err)
	}
	if raw.Format != "raw" {
		return nil, fmt.Errorf("unsupported source format %q (expected qcow2 or raw)", raw.Format)
	}
	return &sourceImageInfo{Format: "raw"}, nil
}

// inspectQcow2Header parses qcow2 header fields without forking qemu-img; (info, true, nil) on qcow2, (nil, false, nil) if not qcow2.
func inspectQcow2Header(path string) (*sourceImageInfo, bool, error) {
	hdr, ok, err := utils.ReadQcow2Header(path)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	if !ok {
		return nil, false, nil
	}
	var compat string
	switch hdr.Version {
	case 2:
		compat = "0.10"
	case 3:
		compat = "1.1"
	default:
		return nil, true, fmt.Errorf("unsupported qcow2 version %d", hdr.Version)
	}
	return &sourceImageInfo{
		Format:         "qcow2",
		Compat:         compat,
		HasBackingFile: hdr.HasBackingFile,
	}, true, nil
}
