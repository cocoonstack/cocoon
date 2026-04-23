package oci

import (
	"context"
	"fmt"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func fetchImage(ctx context.Context, imageRef string) (ref, digestHex string, layers []v1.Layer, err error) {
	logger := log.WithFunc("oci.fetchImage")

	parsedRef, parseErr := name.ParseReference(imageRef)
	if parseErr != nil {
		return "", "", nil, fmt.Errorf("invalid image reference %q: %w", imageRef, parseErr)
	}
	ref = parsedRef.String()

	platform := v1.Platform{
		Architecture: runtime.GOARCH,
		OS:           "linux",
	}

	logger.Debugf(ctx, "Pulling image: %s", ref)

	img, fetchErr := remote.Image(parsedRef,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
	)
	if fetchErr != nil {
		return "", "", nil, fmt.Errorf("fetch image %s: %w", ref, fetchErr)
	}

	manifest, digestErr := img.Digest()
	if digestErr != nil {
		return "", "", nil, fmt.Errorf("get manifest digest: %w", digestErr)
	}
	digestHex = manifest.Hex

	layers, layersErr := img.Layers()
	if layersErr != nil {
		return "", "", nil, fmt.Errorf("get layers: %w", layersErr)
	}
	if len(layers) == 0 {
		return "", "", nil, fmt.Errorf("image %s has no layers", ref)
	}

	return ref, digestHex, layers, nil
}

func isUpToDate(conf *Config, idx *imageIndex, ref, digestHex string) bool {
	entry, ok := idx.Images[ref]
	if !ok || entry == nil || entry.ManifestDigest != images.NewDigest(digestHex) {
		return false
	}
	if !utils.ValidFile(conf.KernelPath(entry.KernelLayer.Hex())) ||
		!utils.ValidFile(conf.InitrdPath(entry.InitrdLayer.Hex())) {
		return false
	}
	for _, layer := range entry.Layers {
		if !utils.ValidFile(conf.BlobPath(layer.Digest.Hex())) {
			return false
		}
	}
	return true
}

func collectBootHexes(idx *imageIndex) map[string]struct{} {
	hexes := make(map[string]struct{})
	for _, e := range idx.Images {
		if e == nil {
			continue
		}
		if e.KernelLayer != "" {
			hexes[e.KernelLayer.Hex()] = struct{}{}
		}
		if e.InitrdLayer != "" {
			hexes[e.InitrdLayer.Hex()] = struct{}{}
		}
	}
	return hexes
}
