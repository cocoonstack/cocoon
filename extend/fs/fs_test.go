package fs

import (
	"strings"
	"testing"
)

func TestSpecNormalize(t *testing.T) {
	tests := []struct {
		name      string
		spec      Spec
		wantErr   string
		wantNumQ  int
		wantQSize int
	}{
		{name: "minimal valid", spec: Spec{Socket: "/tmp/x.sock", Tag: "share"}, wantNumQ: defaultNumQueues, wantQSize: defaultQueueSize},
		{name: "explicit queues", spec: Spec{Socket: "/tmp/x.sock", Tag: "share", NumQueues: 4, QueueSize: 256}, wantNumQ: 4, wantQSize: 256},
		{name: "missing socket", spec: Spec{Tag: "share"}, wantErr: "socket is required"},
		{name: "relative socket", spec: Spec{Socket: "rel.sock", Tag: "share"}, wantErr: "socket must be absolute"},
		{name: "missing tag", spec: Spec{Socket: "/tmp/x.sock"}, wantErr: "tag is required"},
		{name: "tag with slash", spec: Spec{Socket: "/tmp/x.sock", Tag: "a/b"}, wantErr: "tag"},
		{name: "tag too long", spec: Spec{Socket: "/tmp/x.sock", Tag: strings.Repeat("a", 37)}, wantErr: "tag"},
		{name: "negative queues", spec: Spec{Socket: "/tmp/x.sock", Tag: "share", NumQueues: -1}, wantErr: "non-negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Normalize()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.spec.NumQueues != tt.wantNumQ {
				t.Errorf("NumQueues = %d, want %d", tt.spec.NumQueues, tt.wantNumQ)
			}
			if tt.spec.QueueSize != tt.wantQSize {
				t.Errorf("QueueSize = %d, want %d", tt.spec.QueueSize, tt.wantQSize)
			}
		})
	}
}

func TestDeriveID(t *testing.T) {
	if got := DeriveID("share"); got != "cocoon-fs-share" {
		t.Errorf("DeriveID(share) = %q, want cocoon-fs-share", got)
	}
}
