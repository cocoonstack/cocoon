package images

import (
	"context"

	"golang.org/x/sync/singleflight"

	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/types"
)

// Ops bundles the store and callbacks shared by Inspect/List/Delete; one per backend.
type Ops[I any, E Entry] struct {
	Store      storage.Store[I]
	Type       string
	LookupRefs func(*I, string) []string
	Entries    func(*I) map[string]*E
	Sizer      func(*E) int64
}

// Inspect returns (nil, nil) when no entry matches id.
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

// List returns every image in the index.
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

// SingleflightDo collapses concurrent same-key operations (e.g. pulls) into one execution.
func SingleflightDo(g *singleflight.Group, key string, fn func() error) error {
	_, err, _ := g.Do(key, func() (any, error) { return nil, fn() })
	return err
}
