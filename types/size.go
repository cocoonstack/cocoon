package types

import (
	"strings"

	"github.com/docker/go-units"
)

// ParseSize reads a byte size in the Docker (20G, 20GiB) or Kubernetes (20Gi) spelling; every unit is binary, so 20G is 20 GiB.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(strings.ToLower(s), "i") {
		s += "B"
	}
	return units.RAMInBytes(s)
}
