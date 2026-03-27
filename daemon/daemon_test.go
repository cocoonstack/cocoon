package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projecteru2/cocoon/config"
	imagebackend "github.com/projecteru2/cocoon/images"
	"github.com/projecteru2/cocoon/service"
)

// testDaemon starts a daemon on a temp unix socket and returns a ready HTTP client + cleanup func.
func testDaemon(t *testing.T) (*http.Client, string, func()) {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "test.sock")

	hyper := &stubHypervisor{}
	imgs := &stubImages{}

	svc := service.NewWithBackends(
		&config.Config{
			RootDir: t.TempDir(),
			RunDir:  t.TempDir(),
			LogDir:  t.TempDir(),
		},
		hyper,
		[]imagebackend.Images{imgs},
		nil, // no network
		nil, // no snapshot
	)

	d, err := New(svc, sock)
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- d.Start(ctx)
	}()

	// Wait for socket to be ready.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			conn.Close() //nolint:errcheck,gosec
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}

	cleanup := func() {
		cancel()
		<-errCh
	}

	return client, "http://localhost", cleanup
}

func TestDaemon_ListVM(t *testing.T) {
	client, base, cleanup := testDaemon(t)
	defer cleanup()

	resp, err := client.Get(base + "/api/v1/vms")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestDaemon_ListImages(t *testing.T) {
	client, base, cleanup := testDaemon(t)
	defer cleanup()

	resp, err := client.Get(base + "/api/v1/images")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestDaemon_GC(t *testing.T) {
	client, base, cleanup := testDaemon(t)
	defer cleanup()

	resp, err := client.Post(base+"/api/v1/gc", "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestDaemon_InspectVM_NotFound(t *testing.T) {
	client, base, cleanup := testDaemon(t)
	defer cleanup()

	resp, err := client.Get(base + "/api/v1/vms/nonexistent")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Should return 500 with error JSON (the stub returns an error for unknown VMs).
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck

	if _, ok := result["error"]; !ok {
		t.Error("expected error field in response")
	}
}

func TestDaemon_StartStopVM(t *testing.T) {
	client, base, cleanup := testDaemon(t)
	defer cleanup()

	body := `{"refs":["vm-1"]}`

	resp, err := client.Post(base+"/api/v1/vms/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
	}

	var result map[string][]string
	json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck

	if len(result["started"]) != 1 || result["started"][0] != "vm-1" {
		t.Errorf("expected started=[vm-1], got %v", result["started"])
	}
}

func TestDaemon_TCPListen(t *testing.T) {
	svc := service.NewWithBackends(
		&config.Config{
			RootDir: t.TempDir(),
			RunDir:  t.TempDir(),
			LogDir:  t.TempDir(),
		},
		&stubHypervisor{},
		nil, nil, nil,
	)

	d, err := New(svc, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create daemon: %v", err)
	}

	// Get the actual bound address (port 0 → auto-assigned).
	addr := d.ListenAddr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- d.Start(ctx)
	}()

	// Wait for the server to start accepting connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			conn.Close() //nolint:errcheck,gosec
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/api/v1/vms", addr))
	if err != nil {
		cancel()
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cancel()
	<-errCh
}
