package hypervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cocoon-sysfs")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "online"), []byte("0-7\n"), 0o600); err != nil {
		panic(err)
	}
	sysCPURoot = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
