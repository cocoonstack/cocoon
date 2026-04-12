package images

import (
	"context"
	"errors"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/types"
)

// ErrAmbiguous is returned by resolution helpers when an image ref
// matches entries in more than one backend (e.g., a name that exists
// in both OCI and cloudimg stores). Callers must disambiguate — the
// package layer will not pick one silently.
var ErrAmbiguous = errors.New("image ref resolves to multiple backends")

type Images interface {
	Type() string

	Pull(context.Context, string, progress.Tracker) error
	Import(ctx context.Context, name string, tracker progress.Tracker, file ...string) error
	Inspect(context.Context, string) (*types.Image, error)
	List(context.Context) ([]*types.Image, error)
	Delete(context.Context, []string) ([]string, error)
	RegisterGC(*gc.Orchestrator)

	Config(context.Context, []*types.VMConfig) ([][]*types.StorageConfig, []*types.BootConfig, error)
}
