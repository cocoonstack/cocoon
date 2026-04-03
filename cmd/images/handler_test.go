package images

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

func gzipWrap(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDetectReader(t *testing.T) {
	qcow2Data := append(qcow2Magic, make([]byte, 100)...)
	tarData := []byte("this is a tar-like stream of data, not really tar but not qcow2 either")

	tests := []struct {
		name     string
		input    []byte
		wantType imageType
		wantData []byte
	}{
		{
			name:     "raw qcow2",
			input:    qcow2Data,
			wantType: imageTypeQcow2,
			wantData: qcow2Data,
		},
		{
			name:     "gzip qcow2",
			input:    gzipWrap(t, qcow2Data),
			wantType: imageTypeQcow2,
			wantData: qcow2Data,
		},
		{
			name:     "raw tar",
			input:    tarData,
			wantType: imageTypeTar,
			wantData: tarData,
		},
		{
			name:     "gzip tar",
			input:    gzipWrap(t, tarData),
			wantType: imageTypeTar,
			wantData: tarData,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, typ, cleanup, err := detectReader(bytes.NewReader(tt.input))
			if err != nil {
				t.Fatalf("detectReader: %v", err)
			}
			defer cleanup()

			if typ != tt.wantType {
				t.Errorf("type: got %d, want %d", typ, tt.wantType)
			}

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, tt.wantData) {
				t.Errorf("data mismatch: got %d bytes, want %d", len(got), len(tt.wantData))
			}
		})
	}
}

func TestDetectReader_TooShort(t *testing.T) {
	_, _, _, err := detectReader(bytes.NewReader([]byte{0x00}))
	if err == nil {
		t.Fatal("expected error for 1-byte input")
	}
}

func TestDetectReader_Empty(t *testing.T) {
	_, _, _, err := detectReader(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestDetectReader_GzipPreservesFullContent(t *testing.T) {
	original := make([]byte, 16384)
	for i := range original {
		original[i] = byte(i % 251)
	}

	gz := gzipWrap(t, original)
	r, typ, cleanup, err := detectReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("detectReader: %v", err)
	}
	defer cleanup()

	if typ != imageTypeTar {
		t.Errorf("type: got %d, want imageTypeTar", typ)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(original))
	}
}
