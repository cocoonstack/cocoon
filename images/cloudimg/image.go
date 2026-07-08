package cloudimg

import (
	"time"

	"github.com/cocoonstack/cocoon/images"
)

type imageIndex struct {
	images.Index[imageEntry]
}

// Lookup finds an entry by URL or content digest (exact or prefix).
func (idx *imageIndex) Lookup(id string) (string, *imageEntry, bool) {
	return images.LookupOne(idx.Images, id)
}

func (idx *imageIndex) LookupRefs(id string) []string {
	return images.LookupRefs(idx.Images, id)
}

type imageEntry struct {
	Ref        string        `json:"ref"`
	ContentSum images.Digest `json:"content_sum"`
	Size       int64         `json:"size"`
	CreatedAt  time.Time     `json:"created_at"`
}

func (e imageEntry) EntryID() string           { return e.ContentSum.String() }
func (e imageEntry) EntryRef() string          { return e.Ref }
func (e imageEntry) EntryCreatedAt() time.Time { return e.CreatedAt }
func (e imageEntry) DigestHexes() []string     { return []string{e.ContentSum.Hex()} }

func imageSizer(e *imageEntry) int64 {
	return e.Size
}
