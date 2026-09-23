package utils

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
)

type pipeStreamReader struct {
	*io.PipeReader
	close func() error
}

func (r *pipeStreamReader) Close() error {
	return r.close()
}

// PipeStream runs write against the pipe's writer in a goroutine; its error closes the pipe and resurfaces on the reader's Close.
func PipeStream(cleanup func(), write func(io.Writer) error) io.ReadCloser {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := write(pw)
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close() //nolint:errcheck,gosec
		}
		done <- err
	}()
	return &pipeStreamReader{
		PipeReader: pr,
		close: sync.OnceValue(func() error {
			err := pr.Close()
			if streamErr := <-done; streamErr != nil {
				err = streamErr
			}
			if cleanup != nil {
				cleanup()
			}
			return err
		}),
	}
}

// TarDirStream streams a directory as a tar archive via a pipe.
func TarDirStream(dir string, cleanup func()) io.ReadCloser {
	return PipeStream(cleanup, func(w io.Writer) error {
		tw := tar.NewWriter(w)
		err := TarDir(tw, dir)
		if closeErr := tw.Close(); err == nil {
			err = closeErr
		}
		return err
	})
}

// TarDirStreamWithRemove streams a directory as tar and removes it after close.
func TarDirStreamWithRemove(dir string) io.ReadCloser {
	return TarDirStream(dir, func() {
		os.RemoveAll(dir) //nolint:errcheck,gosec
	})
}

// PeekReader peeks up to n bytes + returns a reader that re-emits them then the rest; short read at EOF is not an error.
func PeekReader(r io.Reader, n int) ([]byte, io.Reader, error) {
	head := make([]byte, n)
	actual, err := io.ReadFull(r, head)
	head = head[:actual]
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, err
	}
	return head, io.MultiReader(bytes.NewReader(head), r), nil
}
