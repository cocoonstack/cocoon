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

// Inspect returns (nil, nil) when no entry matches id or the id is an
// ambiguous prefix spanning distinct digests (LookupOne semantics).
func (ops Ops[I, E]) Inspect(ctx context.Context, id string) (result *types.Image, err error) {
	err = ops.Store.With(ctx, func(idx *I) error {
		refs := ops.LookupRefs(idx, id)
		if len(refs) == 0 || !refsShareDigest(ops.Entries(idx), refs) {
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
		var delErr error
		deleted, delErr = deleteByID(ctx, ops.Type+".Delete", ops.Entries(idx), func(id string) []string {
			return ops.LookupRefs(idx, id)
		}, ids)
		return delErr
	})
	return deleted, err
}

// SingleflightDo collapses concurrent same-key operations (e.g. pulls) into
// one execution. A waiter's ctx cancellation detaches that waiter only; the
// shared work keeps running under the winner's ctx.
func SingleflightDo(ctx context.Context, g *singleflight.Group, key string, fn func() error) error {
	ch := g.DoChan(key, func() (any, error) { return nil, fn() })
	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}
