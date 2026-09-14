package hypervisor

import "strings"

const (
	// TableRecords and TableNames map onto the legacy file fields in the composition root.
	TableRecords = "records"
	TableNames   = "names"
	// TablePlacements mirrors each record's cpu placement, so a launch tallies rows of one cpu-list instead of every record.
	TablePlacements = "placements"
)

// VMNamespaceName maps a backend type to its meta namespace.
func VMNamespaceName(typ string) string {
	return "vms_" + strings.ReplaceAll(typ, "-", "")
}
