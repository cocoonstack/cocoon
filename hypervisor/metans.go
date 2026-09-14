package hypervisor

import "strings"

const (
	TableRecords = "records"
	TableNames   = "names"
	// TablePlacements mirrors each record's cpu placement, so a launch tallies rows of one cpu-list instead of every record.
	TablePlacements = "placements"
)

// VMNamespaceName maps a backend type to its meta namespace.
func VMNamespaceName(typ string) string {
	return "vms_" + strings.ReplaceAll(typ, "-", "")
}
