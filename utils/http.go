package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"time"
)

const (
	HTTPTimeout = 30 * time.Second
	MaxRetries  = 3
	BaseBackoff = 100 * time.Millisecond
)

// APIError carries the HTTP status code from a REST API response.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// NewSocketHTTPClient creates an HTTP client that dials a Unix socket.
func NewSocketHTTPClient(socketPath string) *http.Client {
	return NewSocketHTTPClientWithTimeout(socketPath, HTTPTimeout)
}

// NewSocketHTTPClientWithTimeout overrides HTTPTimeout for long ops (e.g. snapshot/restore on multi-GiB VMs).
func NewSocketHTTPClientWithTimeout(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

// DoAPI sends a request, validates status, returns body (nil for 204). url must be fully-formed.
func DoAPI(ctx context.Context, hc *http.Client, method, url string, body []byte, expectedStatus int) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	rb, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != expectedStatus {
		msg := fmt.Sprintf("%s %s → %d: %s", method, url, resp.StatusCode, rb)
		if readErr != nil {
			msg += fmt.Sprintf(" (body read error: %v)", readErr)
		}
		return nil, &APIError{Code: resp.StatusCode, Message: msg}
	}
	if readErr != nil {
		return nil, fmt.Errorf("read response body %s %s: %w", method, url, readErr)
	}
	return rb, nil
}

// CheckSocket verifies that a Unix socket is connectable.
func CheckSocket(socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// DoWithRetry retries fn with exponential backoff for transient errors.
func DoWithRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for i := 0; i <= MaxRetries; i++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !IsRetryable(err) {
			return zero, err
		}
		if i < MaxRetries {
			backoff := BaseBackoff * time.Duration(1<<i)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return zero, lastErr
}

// IsRetryable returns true for transient errors (connection failures, 5xx, 429).
func IsRetryable(err error) bool {
	if ae, ok := errors.AsType[*APIError](err); ok {
		return ae.Code >= 500 || ae.Code == http.StatusTooManyRequests
	}
	return true
}

// DoAPIWithRetry wraps DoAPIOnce in DoWithRetry; successCodes[0] is primary, codes[1:] are also accepted (silent nil-body).
func DoAPIWithRetry(ctx context.Context, hc *http.Client, method, url string, body []byte, successCodes ...int) ([]byte, error) {
	return DoWithRetry(ctx, func() ([]byte, error) {
		return DoAPIOnce(ctx, hc, method, url, body, successCodes...)
	})
}

// DoAPIOnce sends a single non-retried request with successCodes defaulting to 204 (codes[1:] are tolerated alts, nil body); use for non-idempotent endpoints where retry would surface as duplicate/conflict (e.g. vm.add-fs, snapshot/create).
func DoAPIOnce(ctx context.Context, hc *http.Client, method, url string, body []byte, successCodes ...int) ([]byte, error) {
	primary := http.StatusNoContent
	if len(successCodes) > 0 {
		primary = successCodes[0]
	}
	resp, apiErr := DoAPI(ctx, hc, method, url, body, primary)
	if apiErr == nil {
		return resp, nil
	}
	if ae, ok := errors.AsType[*APIError](apiErr); ok && len(successCodes) > 1 && slices.Contains(successCodes[1:], ae.Code) {
		return nil, nil
	}
	return nil, apiErr
}

// DoJSONOnce marshals payload and sends it via DoAPIOnce; kind names the payload in the marshal error.
func DoJSONOnce[T any](ctx context.Context, hc *http.Client, method, url, kind string, payload T, successCodes ...int) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", kind, err)
	}
	return DoAPIOnce(ctx, hc, method, url, body, successCodes...)
}

// DoJSONWithRetry is DoJSONOnce under DoWithRetry, for idempotent endpoints.
func DoJSONWithRetry[T any](ctx context.Context, hc *http.Client, method, url, kind string, payload T, successCodes ...int) ([]byte, error) {
	return DoWithRetry(ctx, func() ([]byte, error) {
		return DoJSONOnce(ctx, hc, method, url, kind, payload, successCodes...)
	})
}
