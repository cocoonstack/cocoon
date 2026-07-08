// Package netresize is the runtime interface for resizing a VM's NIC count.
package netresize

import (
	"context"
	"errors"
	"fmt"

	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// ErrUnsupportedBackend signals the resolved hypervisor cannot resize NICs.
var ErrUnsupportedBackend = errors.New("backend does not support net resize")

// Spec is one resize request.
type Spec struct {
	Target int
}

// Normalize validates the spec.
func (s *Spec) Normalize() error {
	if s.Target < 0 {
		return fmt.Errorf("--nics must be non-negative, got %d", s.Target)
	}
	return nil
}

// NIC is one NIC summary surfaced through Result.Added / Result.Removed.
type NIC struct {
	Index int    `json:"index"`
	TAP   string `json:"tap"`
	MAC   string `json:"mac"`
}

// Result reports the before/after count, NICs touched, and non-fatal Warnings.
type Result struct {
	Before   int      `json:"before"`
	After    int      `json:"after"`
	Added    []NIC    `json:"added,omitempty"`
	Removed  []NIC    `json:"removed,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Plumbing is the host-side network ops NetResize delegates to; network.Network satisfies it.
type Plumbing interface {
	Add(ctx context.Context, vmID string, vmCfg *types.VMConfig, specs ...network.AddSpec) ([]*types.NetworkConfig, error)
	Remove(ctx context.Context, vmID string, indices ...int) error
}

// Resizer resizes a running VM's NIC count.
type Resizer interface {
	NetResize(ctx context.Context, vmRef string, spec Spec, plumbing Plumbing) (Result, error)
}
