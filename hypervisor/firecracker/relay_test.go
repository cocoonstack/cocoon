package firecracker

import (
	"os"
	"strings"
	"testing"
)

func TestWaitRelayLeaseReady(t *testing.T) {
	tests := []struct {
		name    string
		command []byte
		wantErr bool
	}{
		{name: "ready", command: []byte{relayLeaseReadyCommand}},
		{name: "unexpected", command: []byte{'x'}, wantErr: true},
		{name: "closed", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			go func() {
				if len(tt.command) > 0 {
					_, _ = writer.Write(tt.command)
				}
				_ = writer.Close()
			}()
			if err := waitRelayLeaseReady(reader); (err != nil) != tt.wantErr {
				t.Fatalf("waitRelayLeaseReady() error = %v, wantErr %v", err, tt.wantErr)
			}
			_ = reader.Close()
		})
	}
}

func TestWaitRelayLeaseReadyRejectsNilPipe(t *testing.T) {
	if err := waitRelayLeaseReady(nil); err == nil {
		t.Fatal("nil response pipe must fail")
	}
}

func TestWaitRelayLeaseControl(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantRelease bool
	}{
		{name: "confirmed", input: string([]byte{relayCommitLeasesCommand}), wantRelease: true},
		{name: "parent exited"},
		{name: "invalid command", input: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			released, aborted := false, false
			var responses strings.Builder
			waitRelayLeaseControl(strings.NewReader(tt.input), &responses, func() { released = true }, func() { aborted = true })
			if released != tt.wantRelease || aborted == tt.wantRelease {
				t.Fatalf("release=%v abort=%v, want release=%v", released, aborted, tt.wantRelease)
			}
			wantResponse := ""
			if tt.wantRelease {
				wantResponse = string([]byte{relayLeaseCommitAck})
			}
			if got := responses.String(); got != wantResponse {
				t.Fatalf("response=%q, want %q", got, wantResponse)
			}
		})
	}
}
