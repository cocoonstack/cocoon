package oci

import (
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/cocoonstack/cocoon/images"
)

// imageIndex is the top-level structure of the OCI images.json file.
type imageIndex struct {
	images.Index[imageEntry]
}

// Lookup finds an image entry by ref (exact or normalized) or manifest digest.
// Returns the ref key, entry, and whether it was found.
func (idx *imageIndex) Lookup(id string) (string, *imageEntry, bool) {
	if entry, ok := idx.Images[id]; ok && entry != nil {
		return id, entry, true
	}
	// Try normalizing as an image reference (e.g., "ubuntu:24.04" -> "docker.io/library/ubuntu:24.04").
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

// LookupRefs returns all ref keys matching id for DeleteByID.
// Delegates to shared images.LookupRefs with OCI reference normalization.
func (idx *imageIndex) LookupRefs(id string) []string {
	return images.LookupRefs(idx.Images, id, func(s string) (string, bool) {
		parsed, err := name.ParseReference(s)
		if err != nil {
			return "", false
		}
		return parsed.String(), true
	})
}

// imageEntry records one pulled OCI image.
// Paths are not stored; they are derived from digests and config at runtime.
type imageEntry struct {
	Ref            string        `json:"ref"`
	ManifestDigest images.Digest `json:"manifest_digest"`
	Layers         []layerEntry  `json:"layers"`
	KernelLayer    images.Digest `json:"kernel_layer"` // digest of layer containing vmlinuz
	InitrdLayer    images.Digest `json:"initrd_layer"` // digest of layer containing initrd.img
	Size           int64         `json:"size"`         // total on-disk size of all artifacts
	CreatedAt      time.Time     `json:"created_at"`
}

// images.Entry implementation (value receivers).

// EntryID returns the manifest digest as the unique entry identifier.
func (e imageEntry) EntryID() string { return e.ManifestDigest.String() }

// EntryRef returns the image reference string.
func (e imageEntry) EntryRef() string { return e.Ref }

// EntryCreatedAt returns when this image entry was created.
func (e imageEntry) EntryCreatedAt() time.Time { return e.CreatedAt }

// DigestHexes returns the hex-encoded digests of all layers.
func (e imageEntry) DigestHexes() []string {
	hexes := make([]string, len(e.Layers))
	for i, l := range e.Layers {
		hexes[i] = l.Digest.Hex()
	}
	return hexes
}

// layerEntry records one EROFS layer within an image.
type layerEntry struct {
	Digest images.Digest `json:"digest"`
}
