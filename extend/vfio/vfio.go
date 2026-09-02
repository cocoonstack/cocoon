// Package vfio is the runtime attach interface for VFIO PCI passthrough (GPU, NIC, NVMe); attach is runtime-only, and host IOMMU + vfio-pci binding are the user's job.
package vfio

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// SysfsPCIPrefix is the canonical host path for a PCI device.
const SysfsPCIPrefix = "/sys/bus/pci/devices/"

var (
	// ErrUnsupportedBackend signals the backend cannot hot-plug VFIO devices (e.g. Firecracker).
	ErrUnsupportedBackend = errors.New("backend does not support device attach")

	// Match BDF in either short (01:00.0) or full (0000:01:00.0) form so the CLI accepts what `lspci` prints by default.
	bdfShortRe = regexp.MustCompile(`^[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	bdfFullRe  = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

	// User-facing id charset matches CH device-id constraints; the prefix "cocoon-" is reserved so cocoon-derived ids never collide.
	validIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// Spec is one attach request. PCI may be a short BDF, full BDF, or a sysfs path; normalizePath canonicalizes it.
type Spec struct {
	PCI string
	ID  string
}

// NormalizedPath validates the spec and returns the canonical sysfs path; existence is checked at attach time.
func (s *Spec) NormalizedPath() (string, error) {
	if s.PCI == "" {
		return "", fmt.Errorf("pci is required")
	}
	if s.ID != "" && (strings.HasPrefix(s.ID, "cocoon-") || !validIDRe.MatchString(s.ID)) {
		return "", fmt.Errorf("id %q invalid: must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63} and not start with cocoon-", s.ID)
	}
	return normalizePath(s.PCI)
}

// Attached is the inspect-time view of one VFIO device from running VM state.
type Attached struct {
	ID  string `json:"id"`
	BDF string `json:"bdf,omitempty"`
}

// Attacher hot-plugs and removes VFIO PCI passthrough devices.
type Attacher interface {
	DeviceAttach(ctx context.Context, vmRef string, spec Spec) (deviceID string, err error)
	DeviceDetach(ctx context.Context, vmRef, id string) error
}

// Lister enumerates VFIO devices from running VM state.
type Lister interface {
	DeviceList(ctx context.Context, vmRef string) ([]Attached, error)
}

// normalizePath maps {short BDF, full BDF, sysfs path} → canonical /sys/bus/pci/devices/<bdf>; rejects paths outside that root.
func normalizePath(input string) (string, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return "", fmt.Errorf("empty pci path")
	}
	if strings.HasPrefix(in, "/") {
		bdf, ok := strings.CutPrefix(filepath.Clean(in), SysfsPCIPrefix)
		if !ok {
			return "", fmt.Errorf("pci %q invalid: absolute path must be under %s", input, SysfsPCIPrefix)
		}
		bdf = strings.ToLower(bdf)
		if !bdfFullRe.MatchString(bdf) {
			return "", fmt.Errorf("pci %q invalid: sysfs BDF suffix must match 0000:xx:yy.z", input)
		}
		return SysfsPCIPrefix + bdf, nil
	}
	low := strings.ToLower(in)
	switch {
	case bdfFullRe.MatchString(low):
		return SysfsPCIPrefix + low, nil
	case bdfShortRe.MatchString(low):
		return SysfsPCIPrefix + "0000:" + low, nil
	}
	return "", fmt.Errorf("pci %q invalid: expect BDF (01:00.0 / 0000:01:00.0) or sysfs path", input)
}
