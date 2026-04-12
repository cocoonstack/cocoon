package cloudimg

import (
	"time"

	"github.com/cocoonstack/cocoon/images"
)

// imageIndex is the top-level structure of the cloudimg images.json file.
type imageIndex struct {
	images.Index[imageEntry]
}

// Lookup finds an image entry by URL or content digest.
// Returns the ref key, entry, and whether it was found.
func (idx *imageIndex) Lookup(id string) (string, *imageEntry, bool) {
	// Exact URL match.
	if entry, ok := idx.Images[id]; ok && entry != nil {
		return id, entry, true
	}
	// Search by content digest.
	for ref, entry := range idx.Images {
		if entry != nil && (entry.ContentSum.String() == id || entry.ContentSum.Hex() == id) {
			return ref, entry, true
		}
	}
	return "", nil, false
}

// LookupRefs returns all ref keys matching id for DeleteByID.
// Delegates to shared images.LookupRefs (no normalizers needed for URLs).
func (idx *imageIndex) LookupRefs(id string) []string {
	return images.LookupRefs(idx.Images, id)
}

// imageEntry records one pulled cloud image.
type imageEntry struct {
	Ref        string        `json:"ref"`         // Original URL.
	ContentSum images.Digest `json:"content_sum"` // SHA-256 of downloaded content.
	Size       int64         `json:"size"`        // qcow2 blob size on disk.
	CreatedAt  time.Time     `json:"created_at"`
}

// images.Entry implementation (value receivers).

// EntryID returns the content checksum as the unique entry identifier.
func (e imageEntry) EntryID() string { return e.ContentSum.String() }

// EntryRef returns the image reference string.
func (e imageEntry) EntryRef() string { return e.Ref }

// EntryCreatedAt returns when this image entry was created.
func (e imageEntry) EntryCreatedAt() time.Time { return e.CreatedAt }

// DigestHexes returns the hex-encoded content digest.
func (e imageEntry) DigestHexes() []string { return []string{e.ContentSum.Hex()} }

func imageSizer(e *imageEntry) int64 {
	return e.Size
}
