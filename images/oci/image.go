package oci

import (
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/cocoonstack/cocoon/images"
)

type imageIndex = images.Index[imageEntry]

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

// normalizeRef expands a short OCI ref (e.g. "ubuntu:24.04" → "docker.io/library/ubuntu:24.04").
func normalizeRef(s string) (string, bool) {
	parsed, err := name.ParseReference(s)
	if err != nil {
		return "", false
	}
	return parsed.String(), true
}
