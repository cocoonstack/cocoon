//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package vmlock

import (
	"context"
	"fmt"
	"os"
)

// SharedLease is unavailable on platforms without flock inheritance.
type SharedLease struct{}

// NewSharedLease reports that shared VM leases are unavailable.
func NewSharedLease(context.Context, string, string) (*SharedLease, error) {
	return nil, fmt.Errorf("shared VM leases are unsupported on this platform")
}

// File returns no inheritable descriptor on unsupported platforms.
func (*SharedLease) File() *os.File { return nil }

// Close is a no-op on unsupported platforms.
func (*SharedLease) Close() error { return nil }
