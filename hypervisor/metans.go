package hypervisor

import "strings"

const (
	// TableRecords and TableNames map onto the legacy file fields in the composition root.
	TableRecords = "records"
	TableNames   = "names"
)

// VMNamespaceName maps a backend type to its meta namespace.
func VMNamespaceName(typ string) string {
	return "vms_" + strings.ReplaceAll(typ, "-", "")
}
