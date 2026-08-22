package cloudimg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/ociutil"
)

const mediaTypeOCIEmptyConfig = "application/vnd.oci.empty.v1+json"

// Uploader is the OCI write surface needed by Pusher.
type Uploader interface {
	HasBlob(ctx context.Context, name, digest string) (bool, error)
	PutBlob(ctx context.Context, name, digest string, body io.Reader, size int64) error
	PutManifest(ctx context.Context, name, tag string, data []byte, contentType string) error
}

// PushOptions describes one standalone qcow2 publication.
type PushOptions struct {
	Name        string
	Tag         string
	Path        string
	Title       string
	Annotations map[string]string
}

// PushResult reports the immutable identity and stored size of a cloud image.
type PushResult struct {
	ManifestDigest string
	TotalSize      int64
}

// Pusher publishes a standalone qcow2 as a Cocoon cloud-image OCI artifact.
type Pusher struct {
	Uploader Uploader
}

func (p *Pusher) Push(ctx context.Context, opts PushOptions) (*PushResult, error) {
	if opts.Name == "" || opts.Path == "" {
		return nil, errors.New("cloudimg push: name and path are required")
	}
	if opts.Tag == "" {
		opts.Tag = "latest"
	}
	if opts.Title == "" {
		opts.Title = filepath.Base(opts.Path)
	}

	f, err := os.Open(opts.Path) //nolint:gosec // path is selected by the local CLI export flow
	if err != nil {
		return nil, fmt.Errorf("open cloud image: %w", err)
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat cloud image: %w", err)
	}
	h := sha256.New()
	if _, hashErr := io.Copy(h, f); hashErr != nil {
		return nil, fmt.Errorf("hash cloud image: %w", hashErr)
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if uploadErr := putBlobIfMissing(ctx, p.Uploader, opts.Name, digest, f, info.Size()); uploadErr != nil {
		return nil, fmt.Errorf("upload cloud image: %w", uploadErr)
	}

	config := []byte("{}")
	configDigest := "sha256:" + ociutil.SHA256Hex(config)
	if configErr := putBlobIfMissing(ctx, p.Uploader, opts.Name, configDigest, bytes.NewReader(config), int64(len(config))); configErr != nil {
		return nil, fmt.Errorf("upload config: %w", configErr)
	}
	annotations := maps.Clone(opts.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[manifest.AnnotationCreated] = time.Now().UTC().Format(time.RFC3339)
	annotations["cocoonstack.disk.format"] = qcow2Format
	m := manifest.OCIManifest{
		SchemaVersion: 2,
		MediaType:     manifest.MediaTypeOCIManifest,
		ArtifactType:  manifest.ArtifactTypeOSImage,
		Config: manifest.Descriptor{
			MediaType: mediaTypeOCIEmptyConfig,
			Digest:    configDigest,
			Size:      int64(len(config)),
		},
		Layers: []manifest.Descriptor{{
			MediaType: manifest.MediaTypeDiskQcow2,
			Digest:    digest,
			Size:      info.Size(),
			Annotations: map[string]string{
				manifest.AnnotationTitle: opts.Title,
			},
		}},
		Annotations: annotations,
	}
	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := p.Uploader.PutManifest(ctx, opts.Name, opts.Tag, manifestBytes, manifest.MediaTypeOCIManifest); err != nil {
		return nil, fmt.Errorf("put manifest %s:%s: %w", opts.Name, opts.Tag, err)
	}
	return &PushResult{
		ManifestDigest: "sha256:" + ociutil.SHA256Hex(manifestBytes),
		TotalSize:      info.Size() + int64(len(config)),
	}, nil
}

func putBlobIfMissing(ctx context.Context, uploader Uploader, name, digest string, body io.ReadSeeker, size int64) error {
	exists, err := uploader.HasBlob(ctx, name, digest)
	if err != nil {
		return fmt.Errorf("check blob %s: %w", digest, err)
	}
	if exists {
		return nil
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek blob %s: %w", digest, err)
	}
	if err := uploader.PutBlob(ctx, name, digest, body, size); err != nil {
		return fmt.Errorf("put blob %s: %w", digest, err)
	}
	return nil
}
