package cgroup

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// SysCPURoot is the sysfs cpu tree ReadTopology walks on a real host.
const SysCPURoot = "/sys/devices/system/cpu"

// Topology is the host's online cpus inside the fence, grouped by the last-level cache they share.
type Topology struct {
	domains [][]int
	cores   [][][]int // per domain, the hardware threads of each physical core
}

// ReadTopology groups the online cpus inside fence by last-level cache; a host without cache sysfs forms one domain.
func ReadTopology(root string, fence []int) (*Topology, error) {
	online, err := readCPUList(filepath.Join(root, "online"))
	if err != nil {
		return nil, err
	}
	if len(fence) > 0 {
		online = slices.DeleteFunc(online, func(c int) bool { return !slices.Contains(fence, c) })
	}
	if len(online) == 0 {
		return nil, errors.New("no online cpu inside the cgroup_cpus fence")
	}
	t := &Topology{}
	if llc := lastCacheIndex(root, online[0]); llc == "" {
		t.domains = [][]int{online}
	} else {
		for rest := online; len(rest) > 0; {
			shared, err := readCPUList(filepath.Join(cpuDir(root, rest[0]), "cache", llc, "shared_cpu_list"))
			if err != nil {
				return nil, err
			}
			inShared := func(c int) bool { return slices.Contains(shared, c) }
			t.domains = append(t.domains, slices.DeleteFunc(slices.Clone(rest), func(c int) bool { return !inShared(c) }))
			rest = slices.DeleteFunc(rest, inShared)
		}
	}
	for _, domain := range t.domains {
		cores, err := coresOf(root, domain)
		if err != nil {
			return nil, err
		}
		t.cores = append(t.cores, cores)
	}
	return t, nil
}

// Place picks the least-loaded domain and, for two or more queues, one hardware thread per queue inside it: distinct cores before SMT siblings, fewer pins than queues once the domain runs out.
func (t *Topology) Place(queues int, load map[int]int) (domain, pins []int) {
	pick := 0
	best := sumLoad(t.domains[0], load)
	for i, d := range t.domains[1:] {
		if l := sumLoad(d, load); l < best {
			pick, best = i+1, l
		}
	}
	domain = slices.Clone(t.domains[pick])
	if queues < 2 {
		return domain, nil
	}
	byLoad := func(a, b int) int { return cmp.Or(cmp.Compare(load[a], load[b]), cmp.Compare(a, b)) }
	layer := make(map[int]int, len(domain))
	ranked := make([]int, 0, len(domain))
	for _, c := range t.cores[pick] {
		core := slices.Clone(c)
		slices.SortFunc(core, byLoad)
		for i, c := range core {
			layer[c] = i
		}
		ranked = append(ranked, core...)
	}
	slices.SortFunc(ranked, func(a, b int) int { return cmp.Or(cmp.Compare(layer[a], layer[b]), byLoad(a, b)) })
	pins = ranked[:min(queues, len(ranked))]
	slices.Sort(pins)
	return domain, pins
}

func coresOf(root string, domain []int) ([][]int, error) {
	var cores [][]int
	seen := make(map[int]bool, len(domain))
	for _, c := range domain {
		if seen[c] {
			continue
		}
		siblings, err := readCPUList(filepath.Join(cpuDir(root, c), "topology", "thread_siblings_list"))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			siblings = []int{c}
		case err != nil:
			return nil, err
		}
		core := slices.DeleteFunc(siblings, func(s int) bool { return !slices.Contains(domain, s) })
		for _, s := range core {
			seen[s] = true
		}
		cores = append(cores, core)
	}
	return cores, nil
}

func sumLoad(cpus []int, load map[int]int) int {
	total := 0
	for _, c := range cpus {
		total += load[c]
	}
	return total
}

func lastCacheIndex(root string, cpu int) string {
	cacheDir := filepath.Join(cpuDir(root, cpu), "cache")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return ""
	}
	best, bestLevel := "", 0
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(cacheDir, e.Name(), "level")) //nolint:gosec // sysfs entry name under a fixed root
		if err != nil {
			continue
		}
		if level, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && level > bestLevel {
			best, bestLevel = e.Name(), level
		}
	}
	return best
}

func cpuDir(root string, cpu int) string {
	return filepath.Join(root, fmt.Sprintf("cpu%d", cpu))
}

func readCPUList(path string) ([]int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // sysfs path under a fixed root
	if err != nil {
		return nil, err
	}
	return ParseCPUList(strings.TrimSpace(string(data)))
}
