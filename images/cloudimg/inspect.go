package cloudimg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

var (
	// qcow2Magic is the qcow2 file signature.
	qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

	// nonImageSignatures catches common payloads qemu-img would misclassify as raw.
	nonImageSignatures = []struct {
		prefix []byte
		desc   string
	}{
		{[]byte("<!"), "content looks like HTML/XML, not a disk image"},
		{[]byte("<?"), "content looks like HTML/XML, not a disk image"},
		{[]byte("<h"), "content looks like HTML, not a disk image"},
		{[]byte("<H"), "content looks like HTML, not a disk image"},
		{[]byte{0x1f, 0x8b}, "content is gzip-compressed (cloudimg does not auto-decompress)"},
		{[]byte("\xfd7zXZ\x00"), "content is xz-compressed (cloudimg does not auto-decompress)"},
		{[]byte("BZh"), "content is bzip2-compressed (cloudimg does not auto-decompress)"},
		{[]byte{0x28, 0xb5, 0x2f, 0xfd}, "content is zstd-compressed (cloudimg does not auto-decompress)"},
		{[]byte("PK"), "content is a zip archive, not a disk image"},
		{[]byte{0x37, 0x7a, 0xbc, 0xaf, 0x27, 0x1c}, "content is a 7z archive, not a disk image"},
	}
)

// IsQcow2File checks whether path starts with the qcow2 magic bytes.
// Short or unreadable files return false.
func IsQcow2File(path string) bool {
	f, err := os.Open(path) //nolint:gosec // path is caller-controlled
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck

	head, err := peekHead(f, len(qcow2Magic))
	if err != nil {
		return false
	}
	return bytes.HasPrefix(head, qcow2Magic)
}

// sniffImageSource rejects obvious non-disk-image prefixes from an open file.
func sniffImageSource(f *os.File) error {
	head, err := peekHead(f, 8)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	return sniffHead(head)
}

// sniffHead rejects obvious non-disk-image prefixes.
func sniffHead(head []byte) error {
	if len(head) < 4 {
		return fmt.Errorf("content too small to be a disk image (%d bytes)", len(head))
	}
	if bytes.HasPrefix(head, qcow2Magic) {
		return nil
	}
	for _, sig := range nonImageSignatures {
		if bytes.HasPrefix(head, sig.prefix) {
			return errors.New(sig.desc)
		}
	}
	return nil
}

// peekHead reads up to n bytes from the start of f without advancing its offset.
func peekHead(f *os.File, n int) ([]byte, error) {
	buf := make([]byte, n)
	m, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:m], nil
}

// sourceImageInfo captures the parts of qemu-img info we care about.
type sourceImageInfo struct {
	Format         string // "qcow2" or "raw"
	Compat         string // qcow2 compat level (e.g. "0.10", "1.1"); empty for non-qcow2
	HasBackingFile bool
}

// inspectImage describes a source image via qemu-img info.
func inspectImage(ctx context.Context, path string) (*sourceImageInfo, error) {
	cmd := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path) //nolint:gosec // path is controlled
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	// Ignore nested protocol-layer formats like "file".
	var raw struct {
		Format          string `json:"format"`
		BackingFilename string `json:"backing-filename"`
		FormatSpecific  struct {
			Data struct {
				Compat string `json:"compat"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse qemu-img info: %w", err)
	}
	if raw.Format != "qcow2" && raw.Format != "raw" {
		return nil, fmt.Errorf("unsupported source format %q (expected qcow2 or raw)", raw.Format)
	}
	return &sourceImageInfo{
		Format:         raw.Format,
		Compat:         raw.FormatSpecific.Data.Compat,
		HasBackingFile: raw.BackingFilename != "",
	}, nil
}
