package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/cocoonstack/cocoon/utils"
)

func streamLog(ctx context.Context, path string, follow bool, tail int) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("open log %s: VM may not have been started yet", path)
		}
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if tail > 0 {
		if seekErr := seekToLastNLines(f, tail); seekErr != nil {
			return fmt.Errorf("seek tail: %w", seekErr)
		}
	}

	if !follow {
		if _, copyErr := io.Copy(os.Stdout, f); copyErr != nil {
			return fmt.Errorf("read log: %w", copyErr)
		}
		return nil
	}

	events, err := utils.WatchFile(ctx, path, logFollowDebounce)
	if err != nil {
		return fmt.Errorf("watch log: %w", err)
	}
	sig, _ := utils.FileHead(f, logHeadSigLen)
	if _, err := io.Copy(os.Stdout, f); err != nil {
		return fmt.Errorf("read log: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			// Sig mismatch catches O_TRUNC re-opens (CH/FC stamp a unique boot timestamp on line 1).
			newSig, _ := utils.FileHead(f, logHeadSigLen)
			if !bytes.Equal(newSig, sig) {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("rewind log: %w", err)
				}
				sig = newSig
			}
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return fmt.Errorf("read log: %w", err)
			}
		}
	}
}

// A final trailing '\n' is not counted as a line separator.
func seekToLastNLines(f *os.File, n int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	const chunk = 4096
	buf := make([]byte, chunk)
	pos, found := size, 0
	for pos > 0 {
		readSize := min(int64(chunk), pos)
		pos -= readSize
		if _, readErr := f.ReadAt(buf[:readSize], pos); readErr != nil {
			return readErr
		}
		for i := readSize - 1; i >= 0; i-- {
			if buf[i] != '\n' || pos+i == size-1 {
				continue
			}
			found++
			if found == n {
				_, seekErr := f.Seek(pos+i+1, io.SeekStart)
				return seekErr
			}
		}
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}
