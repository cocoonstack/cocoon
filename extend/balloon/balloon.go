// Package balloon is the runtime interface for setting a running VM's virtio-balloon target.
package balloon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cocoonstack/cocoon/types"
)

// pageSize is the virtio-balloon unit; a target off it never settles.
const pageSize = 4 << 10

// ErrUnsupported signals the VM cannot take a runtime balloon target: the backend lacks the resize or the VM has no balloon device.
var ErrUnsupported = errors.New("balloon resize unsupported")

// Spec is one resize request: Percent of guest memory when non-zero, else Size bytes.
type Spec struct {
	Size    int64
	Percent int
}

// ParseSpec reads "NN%" of guest memory or a byte size ("2G", "512M", "0").
func ParseSpec(s string) (Spec, error) {
	if pct, ok := strings.CutSuffix(strings.TrimSpace(s), "%"); ok {
		n, err := strconv.Atoi(pct)
		if err != nil || n < 0 || n > 99 {
			return Spec{}, fmt.Errorf("balloon size %q: percent must be an integer from 0 to 99", s)
		}
		return Spec{Percent: n}, nil
	}
	n, err := types.ParseSize(s)
	if err != nil {
		return Spec{}, fmt.Errorf("balloon size %q: %w", s, err)
	}
	return Spec{Size: n}, nil
}

// Resolve returns the target in bytes for a guest of memory bytes, rounded down to whole balloon pages.
func (s Spec) Resolve(memory int64) (int64, error) {
	size := s.Size
	if s.Percent > 0 {
		size = memory * int64(s.Percent) / 100
	}
	if size < 0 || size >= memory {
		return 0, fmt.Errorf("balloon size %d must be below guest memory %d", size, memory)
	}
	return size &^ (pageSize - 1), nil
}

// Status is the live balloon state; a nil Actual means the backend cannot report it.
type Status struct {
	Target  int64  `json:"target"`
	Actual  *int64 `json:"actual,omitzero"`
	Default int64  `json:"default"`
}

// Resizer sets and reads a running VM's balloon target.
type Resizer interface {
	BalloonResize(ctx context.Context, vmRef string, spec Spec) (Status, error)
	// BalloonStatus returns nil (not error) for stopped VMs and VMs without a balloon device so inspect can omit the field.
	BalloonStatus(ctx context.Context, vmRef string) (*Status, error)
}
