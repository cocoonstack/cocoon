package network

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/cocoonstack/cocoon/utils"
)

const (
	// netQueueSize is the default virtio-net ring depth per queue.
	netQueueSize = 512

	vmIDPrefixLen = 8
)

// NetNumQueues returns the virtio-net queue count for cpu; CH uses TX+RX pairs, so the result is always even (>= 2).
func NetNumQueues(cpu int) int {
	return 2 * max(cpu, 1)
}

func ResolveQueueSize(qs int) int {
	return cmp.Or(qs, netQueueSize)
}

func ResolveQueues(specQueues, cpu int) int {
	return cmp.Or(specQueues, NetNumQueues(cpu))
}

func VMIDPrefix(vmID string) string {
	return vmID[:min(len(vmID), vmIDPrefixLen)]
}

func TAPName(prefix, vmID string, nic int) string {
	return fmt.Sprintf("%s%s-%d", prefix, VMIDPrefix(vmID), nic)
}

func ParseTAPName(tapPrefix, name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, tapPrefix)
	if !ok {
		return "", false
	}
	vmPrefix, _, _ := strings.Cut(rest, "-")
	nic, ok := TAPIndex(rest)
	if !ok || len(vmPrefix) != vmIDPrefixLen || !utils.ValidIDPrefix(vmPrefix) || TAPName(tapPrefix, vmPrefix, nic) != name {
		return "", false
	}
	return vmPrefix, true
}

func TAPIndex(tapName string) (int, bool) {
	_, after, ok := strings.CutLast(tapName, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(after)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
