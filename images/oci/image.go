package oci

import (
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/cocoonstack/cocoon/images"
)

type imageIndex struct {
	images.Index[imageEntry]
}

// Lookup finds an entry by ref (exact or normalized) or manifest digest.
func (idx *imageIndex) Lookup(id string) (string, *imageEntry, bool) {
	if entry, ok := idx.Images[id]; ok && entry != nil {
		return id, entry, true
	}
	if parsed, err := name.ParseReference(id); err == nil {
		normalized := parsed.String()
		if entry, ok := idx.Images[normalized]; ok && entry != nil {
			return normalized, entry, true
		}
	}
	for ref, entry := range idx.Images {
		if entry != nil && entry.ManifestDigest.String() == id {
			return ref, entry, true
		}
	}
	return "", nil, false
}

func (idx *imageIndex) LookupRefs(id string) []string {
	return images.LookupRefs(idx.Images, id, func(s string) (string, bool) {
		parsed, err := name.ParseReference(s)
		if err != nil {
			return "", false
		}
		return parsed.String(), true
	})
}

// Paths derive from digests at runtime; not stored.
type imageEntry struct {
	Ref            string        `json:"ref"`
	ManifestDigest images.Digest `json:"manifest_digest"`
	Layers         []layerEntry  `json:"layers"`
	KernelLayer    images.Digest `json:"kernel_layer"`
	InitrdLayer    images.Digest `json:"initrd_layer"`
	Size           int64         `json:"size"`
	CreatedAt      time.Time     `json:"created_at"`
}

func (e imageEntry) EntryID() string           { return e.ManifestDigest.String() }
func (e imageEntry) EntryRef() string          { return e.Ref }
func (e imageEntry) EntryCreatedAt() time.Time { return e.CreatedAt }

func (e imageEntry) DigestHexes() []string {
	hexes := make([]string, len(e.Layers))
	for i, l := range e.Layers {
		hexes[i] = l.Digest.Hex()
	}
	return hexes
}

type layerEntry struct {
	Digest images.Digest `json:"digest"`
}
