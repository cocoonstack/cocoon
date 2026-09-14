package sqlite

import (
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// WAL needs coherent shared memory; these magics mark filesystems that cannot provide it (§4), FUSE refused as unknowable.
var unsupportedFS = map[uint32]string{
	unix.NFS_SUPER_MAGIC:  "nfs",
	unix.CIFS_SUPER_MAGIC: "cifs",
	unix.SMB2_SUPER_MAGIC: "smb2",
	unix.FUSE_SUPER_MAGIC: "fuse",
}

func statfsCheck(dbPath string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(dbPath), &st); err != nil {
		return nil // no statfs answer is not a refusal reason
	}
	if name, ok := unsupportedFS[uint32(st.Type)]; ok { //nolint:gosec // magics fit u32
		return fsRefusal(dbPath, name)
	}
	return nil
}
