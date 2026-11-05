package firecracker

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"testing/synctest"
)

func TestRelaySessionCancelWithBlockedOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		conn, peer := net.Pipe()
		defer peer.Close()
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		bc := &broadcaster{master: reader}
		done := make(chan struct{})
		go func() {
			relaySession(ctx, io.Discard, conn, bc)
			close(done)
		}()
		synctest.Wait()
		go bc.readLoop()
		if _, err := writer.Write([]byte("guest output")); err != nil {
			t.Fatalf("write guest output: %v", err)
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("relay session did not exit with blocked output")
		}
	})
}

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
