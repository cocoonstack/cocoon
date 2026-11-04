package utils

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
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
		pw.CloseWithError(err)
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
		if err := TarDir(tw, dir); err != nil {
			return err
		}
		return tw.Close()
	})
}

// PeekReader returns up to n head bytes and a reader that replays them before the rest; a short read at EOF is not an error.
func PeekReader(r io.Reader, n int) ([]byte, io.Reader, error) {
	head := make([]byte, n)
	actual, err := io.ReadFull(r, head)
	head = head[:actual]
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil, err
	}
	return head, io.MultiReader(bytes.NewReader(head), r), nil
}
