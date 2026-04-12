package utils

import (
	"archive/tar"
	"io"
	"os"
	"sync"
)

// PipeStreamReader wraps a PipeReader with background error collection and cleanup.
type PipeStreamReader struct {
	*io.PipeReader
	done    <-chan error
	once    sync.Once
	err     error
	cleanup func()
}

// NewPipeStreamReader creates a PipeStreamReader that waits for the producer goroutine on close.
func NewPipeStreamReader(pr *io.PipeReader, done <-chan error, cleanup func()) *PipeStreamReader {
	return &PipeStreamReader{PipeReader: pr, done: done, cleanup: cleanup}
}

// Close drains the producer, runs cleanup, and returns any accumulated error.
func (r *PipeStreamReader) Close() error {
	r.once.Do(func() {
		r.err = r.PipeReader.Close()
		if streamErr := <-r.done; streamErr != nil {
			r.err = streamErr
		}
		if r.cleanup != nil {
			r.cleanup()
		}
	})
	return r.err
}

// TarDirStream streams a directory as a tar archive via a pipe.
func TarDirStream(dir string, cleanup func()) io.ReadCloser {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var streamErr error
		defer func() {
			if streamErr != nil {
				pw.CloseWithError(streamErr) //nolint:errcheck,gosec
			} else {
				pw.Close() //nolint:errcheck,gosec
			}
			done <- streamErr
		}()

		tw := tar.NewWriter(pw)
		streamErr = TarDir(tw, dir)
		if closeErr := tw.Close(); streamErr == nil {
			streamErr = closeErr
		}
	}()

	return NewPipeStreamReader(pr, done, cleanup)
}

// TarDirStreamWithRemove streams a directory as tar and removes it after close.
func TarDirStreamWithRemove(dir string) io.ReadCloser {
	return TarDirStream(dir, func() {
		os.RemoveAll(dir) //nolint:errcheck,gosec
	})
}
