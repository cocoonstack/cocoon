package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIError_Error(t *testing.T) {
	ae := &APIError{Code: 500, Message: "internal"}
	if ae.Error() != "internal" {
		t.Errorf("expected %q, got %q", "internal", ae.Error())
	}
}

func TestNewSocketHTTPClient_DialsSocket(t *testing.T) {
	sockPath, ln := listenUnix(t, "test")

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		if !strings.Contains(string(buf[:n]), "GET /ping") {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\npong"))
	}()

	hc := NewSocketHTTPClient(sockPath)
	resp, err := hc.Get("http://localhost/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Errorf("expected %q, got %q", "pong", string(body))
	}
}

func TestNewSocketHTTPClient_BadSocket(t *testing.T) {
	hc := NewSocketHTTPClient("/nonexistent/socket.sock")
	resp, err := hc.Get("http://localhost/ping")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected error for nonexistent socket")
	}
}

func TestDoAPI_Success_GET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			t.Errorf("expected no Content-Type for GET without body, got %q", ct)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	body, err := DoAPI(t.Context(), srv.Client(), http.MethodGet, srv.URL+"/test", nil, http.StatusOK)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != `{"status":"ok"}` {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestDoAPI_Success_PUT_NoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	body, err := DoAPI(t.Context(), srv.Client(), http.MethodPut, srv.URL+"/shutdown", nil, http.StatusNoContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body for 204, got %q", body)
	}
}

func TestDoAPI_PUT_WithBody_SetsContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", ct)
		}
		reqBody, _ := io.ReadAll(r.Body)
		if string(reqBody) != `{"key":"val"}` {
			t.Errorf("unexpected request body: %s", reqBody)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, err := DoAPI(t.Context(), srv.Client(), http.MethodPut, srv.URL+"/test", []byte(`{"key":"val"}`), http.StatusNoContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoAPI_StatusMismatch_ReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, err := DoAPI(t.Context(), srv.Client(), http.MethodGet, srv.URL+"/test", nil, http.StatusOK)
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := errors.AsType[*APIError](err)
	if !ok {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if ae.Code != http.StatusInternalServerError {
		t.Errorf("expected code 500, got %d", ae.Code)
	}
	if !strings.Contains(ae.Message, "boom") {
		t.Errorf("expected message to contain 'boom', got %q", ae.Message)
	}
}

func TestDoAPI_ConnectionError(t *testing.T) {
	hc := &http.Client{Timeout: 100 * time.Millisecond}
	_, err := DoAPI(t.Context(), hc, http.MethodGet, "http://127.0.0.1:1/nope", nil, http.StatusOK)
	if err == nil {
		t.Fatal("expected connection error")
	}
	if ae, ok := errors.AsType[*APIError](err); ok {
		t.Errorf("expected non-APIError, got APIError{%d}", ae.Code)
	}
}

func TestDoAPI_InvalidURL(t *testing.T) {
	_, err := DoAPI(t.Context(), http.DefaultClient, http.MethodGet, "://bad", nil, http.StatusOK)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestDoAPI_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := DoAPI(ctx, srv.Client(), http.MethodGet, srv.URL+"/slow", nil, http.StatusOK)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestCheckSocket_Success(t *testing.T) {
	sockPath, _ := listenUnix(t, "check")

	if err := CheckSocket(sockPath); err != nil {
		t.Errorf("expected success, got: %v", err)
	}
}

func TestCheckSocket_NotExist(t *testing.T) {
	err := CheckSocket("/nonexistent/test.sock")
	if err == nil {
		t.Fatal("expected error for nonexistent socket")
	}
}

func TestCheckSocket_NotSocket(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notsock")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	f.Close()

	if err := CheckSocket(f.Name()); err == nil {
		t.Fatal("expected error for non-socket file")
	}
}

func TestDoWithRetry_SucceedsAfterFailures(t *testing.T) {
	tests := []struct {
		name     string
		failures int
		err      error
	}{
		{"first attempt", 0, nil},
		{"transient error", 2, fmt.Errorf("transient error")},
		{"retryable API error", 2, &APIError{Code: 500, Message: "server error"}},
		{"429 is retryable", 1, &APIError{Code: 429, Message: "rate limited"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			result, err := DoWithRetry(t.Context(), func() (int, error) {
				calls++
				if calls <= tt.failures {
					return 0, tt.err
				}
				return 42, nil
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != 42 {
				t.Errorf("expected 42, got %d", result)
			}
			if calls != tt.failures+1 {
				t.Errorf("expected %d calls, got %d", tt.failures+1, calls)
			}
		})
	}
}

func TestDoWithRetry_ExhaustedRetries(t *testing.T) {
	calls := 0
	_, err := DoWithRetry(t.Context(), func() (string, error) {
		calls++
		return "", fmt.Errorf("always fails")
	})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if calls != maxRetries+1 {
		t.Errorf("expected %d calls, got %d", maxRetries+1, calls)
	}
}

func TestDoWithRetry_NonRetryableError_StopsImmediately(t *testing.T) {
	calls := 0
	_, err := DoWithRetry(t.Context(), func() (string, error) {
		calls++
		return "", &APIError{Code: 404, Message: "not found"}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (non-retryable), got %d", calls)
	}
	if ae, ok := errors.AsType[*APIError](err); !ok || ae.Code != 404 {
		t.Errorf("expected APIError{404}, got %v", err)
	}
}

func TestDoWithRetry_ContextCanceled_DuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := DoWithRetry(ctx, func() (string, error) {
		return "", fmt.Errorf("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"400", &APIError{Code: 400}, false},
		{"401", &APIError{Code: 401}, false},
		{"403", &APIError{Code: 403}, false},
		{"404", &APIError{Code: 404}, false},
		{"409", &APIError{Code: 409}, false},
		{"422", &APIError{Code: 422}, false},
		{"500", &APIError{Code: 500}, true},
		{"502", &APIError{Code: 502}, true},
		{"503", &APIError{Code: 503}, true},
		{"504", &APIError{Code: 504}, true},
		{"429", &APIError{Code: 429}, true},
		{"non-APIError", fmt.Errorf("connection refused"), true},
		{"wrapped 404", fmt.Errorf("outer: %w", &APIError{Code: 404}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("isRetryable = %v, want %v", got, tt.want)
			}
		})
	}
}

func listenUnix(t *testing.T, name string) (string, net.Listener) {
	t.Helper()
	path := filepath.Join("/tmp", fmt.Sprintf("cocoon-%s-%d.sock", name, os.Getpid()))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path, ln
}
