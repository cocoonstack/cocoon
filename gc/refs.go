package gc

import "maps"

// usedBlobIDs is implemented by snapshots that reference image blobs.
type usedBlobIDs interface {
	UsedBlobIDs() map[string]struct{}
}

// activeVMIDs is implemented by snapshots that track live VMs.
type activeVMIDs interface {
	ActiveVMIDs() map[string]struct{}
}

// Collect aggregates ID sets from snapshots via accessor; snapshots that don't implement it are skipped.
func Collect(others map[string]any, accessor func(any) map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for _, snap := range others {
		maps.Copy(result, accessor(snap))
	}
	return result
}

// BlobIDs extracts blob hex IDs from a snapshot; nil if it doesn't implement UsedBlobIDs.
func BlobIDs(snap any) map[string]struct{} {
	if u, ok := snap.(usedBlobIDs); ok {
		return u.UsedBlobIDs()
	}
	return nil
}

// VMIDs extracts active VM IDs from a snapshot; nil if it doesn't implement ActiveVMIDs.
func VMIDs(snap any) map[string]struct{} {
	if a, ok := snap.(activeVMIDs); ok {
		return a.ActiveVMIDs()
	}
	return nil
}
