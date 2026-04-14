package network

const vmIDPrefixLen = 8

// NetNumQueues returns the virtio-net queue count for the given CPU count.
// CH uses queue pairs (TX+RX), so the result is always even (≥ 2).
func NetNumQueues(cpu int) int {
	if cpu <= 1 {
		return 2 //nolint:mnd
	}
	return cpu * 2 //nolint:mnd
}

// VMIDPrefix returns the first 8 characters of a VM ID, matching the
// truncation used by both bridge and CNI TAP device naming.
func VMIDPrefix(vmID string) string {
	if len(vmID) > vmIDPrefixLen {
		return vmID[:vmIDPrefixLen]
	}
	return vmID
}
