package images

import (
	"context"

	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/types"
)

// Ops bundles the store and callbacks shared by Inspect/List/Delete.
// Each backend creates one Ops instance during initialization.
type Ops[I any, E Entry] struct {
	Store      storage.Store[I]
	Type       string
	LookupRefs func(*I, string) []string
	Entries    func(*I) map[string]*E
	Sizer      func(*E) int64
}

// Inspect reads one entry by id and converts it to types.Image.
// Returns (nil, nil) when no entry matches.
func (ops Ops[I, E]) Inspect(ctx context.Context, id string) (result *types.Image, err error) {
	err = ops.Store.With(ctx, func(idx *I) error {
		refs := ops.LookupRefs(idx, id)
		if len(refs) == 0 {
			return nil
		}
		result = entryToImage(ops.Entries(idx)[refs[0]], ops.Type, ops.Sizer)
		return nil
	})
	return result, err
}

// List reads all entries and converts them to []types.Image.
func (ops Ops[I, E]) List(ctx context.Context) (result []*types.Image, err error) {
	err = ops.Store.With(ctx, func(idx *I) error {
		result = listImages(ops.Entries(idx), ops.Type, ops.Sizer)
		return nil
	})
	return result, err
}

// Delete deletes entries from an index by ids and returns removed refs.
func (ops Ops[I, E]) Delete(ctx context.Context, ids []string) (deleted []string, err error) {
	err = ops.Store.Update(ctx, func(idx *I) error {
		deleted = deleteByID(ctx, ops.Type+".Delete", ops.Entries(idx), func(id string) []string {
			return ops.LookupRefs(idx, id)
		}, ids)
		return nil
	})
	return deleted, err
}
