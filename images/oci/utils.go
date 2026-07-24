package oci

import (
	"fmt"
	"os"
)

func newWorkDir(conf *Config, pattern string) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp(conf.TempDir(), pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create work dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
