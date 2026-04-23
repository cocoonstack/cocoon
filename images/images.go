package images

import (
	"context"
	"errors"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/types"
)

var (
	// ErrAmbiguous reports an image ref that matches more than one backend.
	ErrAmbiguous = errors.New("image ref resolves to multiple backends")
)

// Images defines the interface for an image backend (OCI, cloudimg).
type Images interface {
	Type() string

	Pull(ctx context.Context, ref string, force bool, tracker progress.Tracker) error
	Import(ctx context.Context, name string, tracker progress.Tracker, file ...string) error
	Inspect(context.Context, string) (*types.Image, error)
	List(context.Context) ([]*types.Image, error)
	Delete(context.Context, []string) ([]string, error)
	RegisterGC(*gc.Orchestrator)

	Config(context.Context, []*types.VMConfig) ([][]*types.StorageConfig, []*types.BootConfig, error)
}
