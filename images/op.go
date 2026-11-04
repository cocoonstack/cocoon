package images

import (
	"context"
	"os"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Ops bundles the store and type shared by Inspect/List/Delete; one per backend.
type Ops[E Entry] struct {
	Store           *Store[E]
	Typ             string
	Base            *BaseConfig
	PinnedElsewhere PinRecheck
}

func (ops *Ops[E]) Type() string { return ops.Typ }

func (ops *Ops[E]) PinBlobs(_ context.Context, blobIDs map[string]struct{}) (func(), error) {
	return PinBlobs(ops.Base, blobIDs)
}

func (ops *Ops[E]) OwnsBlob(hex string) bool { return ops.Base.OwnsBlob(hex) }

func (ops *Ops[E]) GCConfig() GCModuleConfig[E] {
	return GCModuleConfig[E]{
		Name:     ops.Typ,
		Store:    ops.Store,
		LockPath: ops.Base.BlobLockPath,
		ReadRefs: ReferencedDigests[E],
		ScanDisk: func() ([]string, error) { return utils.ScanFileStems(ops.Base.BlobsDir(), ops.Base.BlobExt) },
		Removers: []BlobRemover{
			func(hex string) error { return os.Remove(ops.Base.BlobPath(hex)) },
		},
		TempDir:         ops.Base.TempDir(),
		PinnedElsewhere: ops.PinnedElsewhere,
	}
}

// Inspect returns (nil, nil) when no entry matches id or the id is an ambiguous prefix spanning distinct digests (LookupOne semantics).
func (ops *Ops[E]) Inspect(ctx context.Context, id string) (result *types.Image, err error) {
	err = ops.Store.View(ctx, func(idx *Index[E]) error {
		if _, entry, ok := LookupOne(idx.Images, id); ok {
			result = entryToImage(entry, ops.Typ)
		}
		return nil
	})
	return result, err
}

func (ops *Ops[E]) List(ctx context.Context) (result []*types.Image, err error) {
	err = ops.Store.View(ctx, func(idx *Index[E]) error {
		result = listImages(idx.Images, ops.Typ)
		return nil
	})
	return result, err
}

// Delete removes entries for ids; not-found ids are logged and skipped.
func (ops *Ops[E]) Delete(ctx context.Context, ids []string) (deleted []string, err error) {
	err = ops.Store.Update(ctx, func(idx *Index[E]) error {
		var delErr error
		deleted, delErr = deleteByID(ctx, ops.Typ+".Delete", idx.Images, ids)
		return delErr
	})
	return deleted, err
}
