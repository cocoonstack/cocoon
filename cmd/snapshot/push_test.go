package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	localsnapshot "github.com/cocoonstack/cocoon/snapshot"
)

type exportOnlyBackend struct {
	localsnapshot.Snapshot
	wantRef string
	data    []byte
}

type closeErrorReader struct {
	io.Reader
	err error
}

func (r *closeErrorReader) Close() error { return r.err }

type closeErrorBackend struct {
	localsnapshot.Snapshot
	err error
}

func (b closeErrorBackend) Export(context.Context, string) (io.ReadCloser, error) {
	return &closeErrorReader{Reader: bytes.NewReader(nil), err: b.err}, nil
}

func (b exportOnlyBackend) Export(_ context.Context, ref string) (io.ReadCloser, error) {
	if ref != b.wantRef {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

func TestBackendExporterWaitReturnsCloseError(t *testing.T) {
	want := errors.New("finalize export")
	r, wait, err := (backendExporter{backend: closeErrorBackend{err: want}, ref: "local-snapshot"}).Export(t.Context(), "remote/repository")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); !errors.Is(err, want) {
		t.Fatalf("close error = %v", err)
	}
	if err := wait(); !errors.Is(err, want) {
		t.Fatalf("wait error = %v", err)
	}
}

func TestBackendExporterUsesFixedSourceRef(t *testing.T) {
	b := exportOnlyBackend{wantRef: "local-snapshot", data: []byte("archive")}
	r, wait, err := (backendExporter{backend: b, ref: b.wantRef}).Export(t.Context(), "remote/repository")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive" {
		t.Fatalf("got %q", got)
	}
}
