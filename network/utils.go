package network

import "cmp"

const (
	vmIDPrefixLen = 8

	// NetQueueSize: default virtio-net ring depth per queue; 512 balances throughput vs request latency.
	NetQueueSize = 512
)

// NetNumQueues returns the virtio-net queue count for the given CPU count.
// CH uses queue pairs (TX+RX), so the result is always even (≥ 2).
func NetNumQueues(cpu int) int {
	if cpu <= 1 {
		return 2 //nolint:mnd
	}
	return cpu * 2 //nolint:mnd
}

// ResolveQueueSize returns qs if non-zero, otherwise the default NetQueueSize.
// Negative values aren't reachable from validated callers.
func ResolveQueueSize(qs int) int {
	return cmp.Or(qs, NetQueueSize)
}

// ResolveQueues returns specQueues if set, otherwise the CPU-derived TAP queue count.
func ResolveQueues(specQueues, cpu int) int {
	return cmp.Or(specQueues, NetNumQueues(cpu))
}

// VMIDPrefix returns the first 8 characters of a VM ID, matching the truncation used by both bridge and CNI TAP device naming.
func VMIDPrefix(vmID string) string {
	if len(vmID) > vmIDPrefixLen {
		return vmID[:vmIDPrefixLen]
	}
	return vmID
}
