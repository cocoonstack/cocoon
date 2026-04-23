package gc

// --- Cross-module protocols ---
//
// Each protocol is an unexported interface (implementation detail) paired
// with an exported accessor function. Snapshot types in other packages
// implement the interface by adding the matching method.

// usedBlobIDs is implemented by snapshots that reference image blobs.
type usedBlobIDs interface {
	UsedBlobIDs() map[string]struct{}
}

// activeVMIDs is implemented by snapshots that track live VMs.
type activeVMIDs interface {
	ActiveVMIDs() map[string]struct{}
}

// Collect aggregates ID sets from all snapshots in others using the given
// accessor. Snapshots that don't support the accessor return nil and are
// silently skipped.
//
// Usage:
//
//	blobIDs := gc.Collect(others, gc.BlobIDs)
//	vmIDs   := gc.Collect(others, gc.VMIDs)
func Collect(others map[string]any, accessor func(any) map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for _, snap := range others {
		for id := range accessor(snap) {
			result[id] = struct{}{}
		}
	}
	return result
}

// BlobIDs extracts blob hex IDs from a snapshot.
// Returns nil if the snapshot does not implement UsedBlobIDs.
func BlobIDs(snap any) map[string]struct{} {
	if u, ok := snap.(usedBlobIDs); ok {
		return u.UsedBlobIDs()
	}
	return nil
}

// VMIDs extracts active VM IDs from a snapshot.
// Returns nil if the snapshot does not implement ActiveVMIDs.
func VMIDs(snap any) map[string]struct{} {
	if a, ok := snap.(activeVMIDs); ok {
		return a.ActiveVMIDs()
	}
	return nil
}
