package network

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

const (
	vmIDPrefixLen = 8

	// NetQueueSize: default virtio-net ring depth per queue; 512 balances throughput vs request latency.
	NetQueueSize = 512
)

// NetNumQueues returns the virtio-net queue count for cpu; CH uses TX+RX pairs, so the result is always even (>= 2).
func NetNumQueues(cpu int) int {
	if cpu <= 1 {
		return 2 //nolint:mnd
	}
	return cpu * 2 //nolint:mnd
}

// ResolveQueueSize returns qs if non-zero, otherwise the default NetQueueSize.
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

// TAPName composes the shared TAP device name scheme: prefix + 8-char VM-ID prefix + NIC index.
func TAPName(prefix, vmID string, nic int) string {
	return fmt.Sprintf("%s%s-%d", prefix, VMIDPrefix(vmID), nic)
}

// TAPIndex parses the NIC index back out of a TAPName-scheme device name.
func TAPIndex(tapName string) (int, bool) {
	i := strings.LastIndexByte(tapName, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(tapName[i+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
