package vm

import (
	"net"
	"os"
	"strings"
	"testing"
)

func TestParseExecEnv(t *testing.T) {
	tests := []struct {
		name    string
		pairs   []string
		want    map[string]string
		wantErr string
	}{
		{name: "empty", pairs: nil, want: nil},
		{name: "single", pairs: []string{"FOO=bar"}, want: map[string]string{"FOO": "bar"}},
		{name: "multi", pairs: []string{"A=1", "B=2"}, want: map[string]string{"A": "1", "B": "2"}},
		{name: "value with =", pairs: []string{"URL=https://x?a=1"}, want: map[string]string{"URL": "https://x?a=1"}},
		{name: "empty value allowed", pairs: []string{"K="}, want: map[string]string{"K": ""}},
		{name: "missing =", pairs: []string{"FOO"}, wantErr: "must be KEY=VALUE"},
		{name: "empty key", pairs: []string{"=v"}, wantErr: "must be KEY=VALUE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExecEnv(tt.pairs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got err=%v, want contains %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("got[%q]=%q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestReadHybridVsockReply(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "ok line", input: "OK 1024\n", want: "OK 1024\n"},
		{name: "stops at newline (next bytes preserved)", input: "OK 5\nLEFTOVER", want: "OK 5\n"},
		{name: "no newline EOF", input: "OK 5", wantErr: "EOF"},
		{name: "overflow", input: strings.Repeat("a", hybridVsockReplyMax+1), wantErr: "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readHybridVsockReply(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got err=%v, want contains %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDialHybridVsock_ConnectHandshake drives the CH/FC hybrid vsock dialect (CONNECT <port>\n → OK <port>\n) against an in-process listener.
func TestDialHybridVsock_ConnectHandshake(t *testing.T) {
	// macOS caps unix socket paths at ~104 bytes, so t.TempDir() (long
	// /var/folders/... path) can overflow. Use os.CreateTemp + immediate unlink.
	f, err := os.CreateTemp("", "vsock-*.uds")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	sockPath := f.Name()
	_ = f.Close()
	_ = os.Remove(sockPath)
	defer os.Remove(sockPath) //nolint:errcheck

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	tests := []struct {
		name    string
		reply   string
		wantErr string
	}{
		{name: "OK accepted", reply: "OK 9001\n"},
		{name: "rejected", reply: "Failed\n", wantErr: "Failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				server, aErr := ln.Accept()
				if aErr != nil {
					return
				}
				defer server.Close() //nolint:errcheck
				buf := make([]byte, 64)
				n, _ := server.Read(buf)
				want := "CONNECT 1024\n"
				if string(buf[:n]) != want {
					t.Errorf("server got %q, want %q", buf[:n], want)
				}
				_, _ = server.Write([]byte(tt.reply))
			}()

			conn, err := dialHybridVsock(t.Context(), sockPath, 1024)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got err=%v, want contains %q", err, tt.wantErr)
				}
				<-done
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			_ = conn.Close()
			<-done
		})
	}
}
