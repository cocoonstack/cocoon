package sqlite

import (
	"path/filepath"
	"syscall"
)

// WAL needs coherent shared memory; these magics mark filesystems that
// cannot provide it (§4). FUSE is refused as unknowable.
var unsupportedFS = map[int64]string{
	0x6969:     "nfs",
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x65735546: "fuse",
}

func statfsCheck(dbPath string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(dbPath), &st); err != nil {
		return nil // no statfs answer is not a refusal reason
	}
	if name, ok := unsupportedFS[int64(st.Type)]; ok { //nolint:unconvert // Type width differs per arch
		return fsRefusal(dbPath, name)
	}
	return nil
}
