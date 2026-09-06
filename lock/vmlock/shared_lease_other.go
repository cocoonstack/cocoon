//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package vmlock

import (
	"context"
	"fmt"
	"os"
)

// SharedLease is unavailable on platforms without flock inheritance.
type SharedLease struct{}

func NewSharedLease(context.Context, string, string) (*SharedLease, error) {
	return nil, fmt.Errorf("shared VM leases are unsupported on this platform")
}

func (*SharedLease) File() *os.File { return nil }

func (*SharedLease) Close() error { return nil }
