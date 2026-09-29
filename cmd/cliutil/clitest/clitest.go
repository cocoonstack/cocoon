// Package clitest holds helpers shared by the cmd packages' tests.
package clitest

import (
	"io"
	"os"
	"testing"
)

func CaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		_ = r.Close()
		done <- buf
	}()
	fn()
	_ = w.Close()
	return string(<-done)
}
