package sqlite

import (
	"fmt"
	"os"

	"github.com/cocoonstack/cocoon/meta"
)

// checkFS refuses unsupported filesystems before any WAL work (§4); the env seam lets every entry path (open, init, convert) be asserted in tests.
func checkFS(dbPath string) error {
	if name := os.Getenv("COCOON_TEST_UNSUPPORTED_FS"); name != "" {
		return fsRefusal(dbPath, name)
	}
	return statfsCheck(dbPath)
}

func fsRefusal(dbPath, name string) error {
	return fmt.Errorf("%s sits on %s: WAL needs coherent shared memory, unsupported filesystem: %w", dbPath, name, meta.ErrIO)
}
