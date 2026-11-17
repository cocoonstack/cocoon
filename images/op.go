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

func (o *Ops[E]) Type() string { return o.Typ }

func (o *Ops[E]) PinBlobs(_ context.Context, blobIDs map[string]struct{}) (func(), error) {
	return PinBlobs(o.Base, blobIDs)
}

func (o *Ops[E]) OwnsBlob(hex string) bool { return o.Base.OwnsBlob(hex) }

func (o *Ops[E]) GCConfig() GCModuleConfig[E] {
	return GCModuleConfig[E]{
		Name:     o.Typ,
		Store:    o.Store,
		LockPath: o.Base.BlobLockPath,
		ReadRefs: ReferencedDigests[E],
		ScanDisk: func() ([]string, error) { return utils.ScanFileStems(o.Base.BlobsDir(), o.Base.BlobExt) },
		Removers: []BlobRemover{
			func(hex string) error { return os.Remove(o.Base.BlobPath(hex)) },
		},
		TempDir:         o.Base.TempDir(),
		PinnedElsewhere: o.PinnedElsewhere,
	}
}

// Inspect returns (nil, nil) when no entry matches id or the id is an ambiguous prefix spanning distinct digests (LookupOne semantics).
func (o *Ops[E]) Inspect(ctx context.Context, id string) (result *types.Image, err error) {
	err = o.Store.View(ctx, func(idx *Index[E]) error {
		if _, entry, ok := LookupOne(idx.Images, id); ok {
			result = entryToImage(entry, o.Typ)
		}
		return nil
	})
	return result, err
}

func (o *Ops[E]) List(ctx context.Context) (result []*types.Image, err error) {
	err = o.Store.View(ctx, func(idx *Index[E]) error {
		result = listImages(idx.Images, o.Typ)
		return nil
	})
	return result, err
}

// Delete removes entries for ids; not-found ids are logged and skipped.
func (o *Ops[E]) Delete(ctx context.Context, ids []string) (deleted []string, err error) {
	err = o.Store.Update(ctx, func(idx *Index[E]) error {
		var delErr error
		deleted, delErr = deleteByID(ctx, o.Typ+".Delete", idx.Images, ids)
		return delErr
	})
	return deleted, err
}
