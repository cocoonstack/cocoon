package hypervisor

import "strings"

const (
	// TableRecords/TableNames/TableOrphanDirs name the vms-namespace tables;
	// the composition root maps them onto the legacy file fields.
	TableRecords    = "records"
	TableNames      = "names"
	TableOrphanDirs = "orphandirs"
)

// VMNamespaceName maps a backend type to its meta namespace (design §2).
func VMNamespaceName(typ string) string {
	return "vms_" + strings.ReplaceAll(typ, "-", "")
}
