package images

import godigest "github.com/opencontainers/go-digest"

// Digest represents a content-addressable digest in "algorithm:hex" format (e.g., "sha256:abcdef...").
type Digest string

func NewDigest(hex string) Digest {
	return Digest(godigest.NewDigestFromEncoded(godigest.SHA256, hex))
}

func (d Digest) Hex() string {
	return godigest.Digest(d).Encoded()
}

func (d Digest) String() string {
	return string(d)
}
