//go:build !linux

package bridge

import (
	"context"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/lock/flock"
)

// bridgeSnapshot is a placeholder for non-Linux.
type bridgeSnapshot struct{}

// GCModule returns a no-op GC module on non-Linux — bridge TAPs don't exist.
func GCModule(rootDir string) gc.Module[bridgeSnapshot] {
	return gc.Module[bridgeSnapshot]{
		Name: "bridge",
		// /dev/null is world-writable and supports flock on all Unix platforms,
		// so TryLock always succeeds without creating a real lock file.
		Locker: flock.New("/dev/null"),
		ReadDB: func(_ context.Context) (bridgeSnapshot, error) {
			return bridgeSnapshot{}, nil
		},
		Resolve: func(_ bridgeSnapshot, _ map[string]any) []string {
			return nil
		},
		Collect: func(_ context.Context, _ []string) error {
			return nil
		},
	}
}
