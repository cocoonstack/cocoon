package cloudimg

import (
	"fmt"
	"os"
)

// newTempImage creates a temp image file under conf.TempDir(); cleanup closes and removes it.
func newTempImage(conf *Config, pattern string) (f *os.File, path string, cleanup func(), err error) {
	f, err = os.CreateTemp(conf.TempDir(), pattern)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create temp file: %w", err)
	}
	cleanup = func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return f, f.Name(), cleanup, nil
}
