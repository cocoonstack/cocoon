package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cocoonstack/cocoon/types"
)

func TestReadsWaitForTheFirstPass(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"vms", "/v1/vms", `{"vms":[{"backend":"fake-hv","id":"vm1"`},
		{"events", "/v1/events", "event: sync\ndata: {\"vms\":[{\"backend\":\"fake-hv\",\"id\":\"vm1\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := &Daemon{state: newCache()}
				ctx, cancel := context.WithCancel(t.Context())
				rec, done := serve(ctx, d, tt.path)

				synctest.Wait()
				if rec.Body.Len() != 0 {
					t.Fatalf("answered before the first pass: %q", rec.Body.String())
				}

				st := statusOf("vm1", types.VMStateRunning, 1, true)
				st.Backend = "fake-hv"
				d.state.publish([]VMStatus{st}, true, 0, time.Now())
				synctest.Wait()
				body := rec.Body.String()
				if !strings.HasPrefix(body, tt.want) {
					t.Fatalf("got %q, want prefix %q", body, tt.want)
				}
				if strings.Contains(body, "event: change") {
					t.Fatalf("the first pass replayed as changes after the sync: %q", body)
				}

				cancel()
				<-done
			})
		})
	}
}

func TestAPIShutdownEndsAnEventsStream(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cocoond-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	d := &Daemon{conf: Config{APIAddr: sock}, state: newCache()}
	d.state.publish(nil, true, 0, time.Now())
	ctx, cancel := context.WithCancel(t.Context())
	stop, err := d.serveAPI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Get("http://cocoond/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	start := time.Now()
	stop()
	if took := time.Since(start); took >= apiShutdownTimeout {
		t.Fatalf("shutdown took %s, want the stream ended by the daemon ctx", took)
	}
}

func TestReadsUnblockOnAFailedFirstPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &Daemon{state: newCache()}
		rec, done := serve(t.Context(), d, "/v1/vms")

		synctest.Wait()
		d.state.publish(nil, false, 0, time.Now())
		<-done
		if got := rec.Body.String(); got != "{\"vms\":[]}\n" {
			t.Fatalf("got %q, want the empty list the failed pass published", got)
		}
	})
}

func TestReadsReturnWhenTheClientLeavesBeforeTheFirstPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &Daemon{state: newCache()}
		ctx, cancel := context.WithCancel(t.Context())
		rec, done := serve(ctx, d, "/v1/events")

		synctest.Wait()
		cancel()
		<-done
		if rec.Body.Len() != 0 {
			t.Fatalf("wrote to a client that left before the first pass: %q", rec.Body.String())
		}
	})
}

func serve(ctx context.Context, d *Daemon, path string) (*httptest.ResponseRecorder, <-chan struct{}) {
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.routes().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
	}()
	return rec, done
}
