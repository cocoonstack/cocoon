package cloudimg

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	commoncloudimg "github.com/cocoonstack/cocoon-common/cloudimg"
	"github.com/cocoonstack/cocoon-common/manifest"
)

type fakeUploader struct {
	blobs    map[string][]byte
	manifest []byte
}

func (f *fakeUploader) HasBlob(_ context.Context, _ string, digest string) (bool, error) {
	_, ok := f.blobs[digest]
	return ok, nil
}

func (f *fakeUploader) PutBlob(_ context.Context, _ string, digest string, body io.Reader, _ int64) error {
	b, err := io.ReadAll(body)
	if err == nil {
		f.blobs[digest] = b
	}
	return err
}

func (f *fakeUploader) PutManifest(_ context.Context, _, _ string, data []byte, _ string) error {
	f.manifest = bytes.Clone(data)
	return nil
}

func (f *fakeUploader) ReadBlob(_ context.Context, digest string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.blobs[digest])), nil
}

func TestPusherProducesCloudImageManifest(t *testing.T) {
	path := t.TempDir() + "/disk.qcow2"
	if err := os.WriteFile(path, []byte("qcow2-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploader := &fakeUploader{blobs: map[string][]byte{}}
	result, err := (&Pusher{Uploader: uploader}).Push(t.Context(), PushOptions{
		Name: "team/image",
		Tag:  "v1",
		Path: path,
		Annotations: map[string]string{
			"cocoonstack.disk.format": "raw",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestDigest == "" {
		t.Fatal("manifest digest is empty")
	}
	var got manifest.OCIManifest
	if err := json.Unmarshal(uploader.manifest, &got); err != nil {
		t.Fatal(err)
	}
	if got.ArtifactType != manifest.ArtifactTypeOSImage || len(got.Layers) != 1 {
		t.Fatalf("manifest artifact=%q layers=%d", got.ArtifactType, len(got.Layers))
	}
	if got.Layers[0].MediaType != manifest.MediaTypeDiskQcow2 || got.Layers[0].Title() != "disk.qcow2" {
		t.Fatalf("unexpected disk descriptor: %+v", got.Layers[0])
	}
	if got.Annotations["cocoonstack.disk.format"] != "qcow2" {
		t.Fatalf("disk format annotation = %q", got.Annotations["cocoonstack.disk.format"])
	}
	var pulled bytes.Buffer
	if err := commoncloudimg.Stream(t.Context(), uploader.manifest, uploader, &pulled); err != nil {
		t.Fatal(err)
	}
	if pulled.String() != "qcow2-data" {
		t.Fatalf("pulled cloud image = %q", pulled.String())
	}
}
