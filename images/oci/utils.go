package oci

import (
	"fmt"
	"os"
)

// newWorkDir creates a scratch dir under conf.TempDir(); cleanup removes it recursively.
func newWorkDir(conf *Config, pattern string) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp(conf.TempDir(), pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create work dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
