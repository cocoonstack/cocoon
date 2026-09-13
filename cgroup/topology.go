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
	root    string
	domains [][]int
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
	t := &Topology{root: root}
	llc := lastCacheIndex(root, online[0])
	for rest := online; len(rest) > 0; {
		domain := rest
		if llc != "" {
			shared, err := readCPUList(filepath.Join(cpuDir(root, rest[0]), "cache", llc, "shared_cpu_list"))
			if err != nil {
				return nil, err
			}
			domain = slices.DeleteFunc(slices.Clone(rest), func(c int) bool { return !slices.Contains(shared, c) })
		}
		t.domains = append(t.domains, domain)
		rest = slices.DeleteFunc(slices.Clone(rest), func(c int) bool { return slices.Contains(domain, c) })
	}
	return t, nil
}

// Place picks the least-loaded domain and, for two or more queues, one hardware thread per queue inside it: distinct cores before SMT siblings, fewer pins than queues once the domain runs out.
func (t *Topology) Place(queues int, load map[int]int) (domain, pins []int, err error) {
	domain = t.domains[0]
	best := sumLoad(domain, load)
	for _, d := range t.domains[1:] {
		if l := sumLoad(d, load); l < best {
			domain, best = d, l
		}
	}
	if queues < 2 {
		return domain, nil, nil
	}
	cores, err := t.cores(domain)
	if err != nil {
		return nil, nil, err
	}
	byLoad := func(a, b int) int { return cmp.Or(cmp.Compare(load[a], load[b]), cmp.Compare(a, b)) }
	for _, core := range cores {
		slices.SortFunc(core, byLoad)
	}
	for layer := 0; len(pins) < queues; layer++ {
		var candidates []int
		for _, core := range cores {
			if layer < len(core) {
				candidates = append(candidates, core[layer])
			}
		}
		if len(candidates) == 0 {
			break
		}
		slices.SortFunc(candidates, byLoad)
		pins = append(pins, candidates[:min(len(candidates), queues-len(pins))]...)
	}
	slices.Sort(pins)
	return domain, pins, nil
}

// cores splits domain into its SMT sibling sets; a host without topology sysfs counts every cpu as its own core.
func (t *Topology) cores(domain []int) ([][]int, error) {
	var cores [][]int
	seen := make(map[int]bool, len(domain))
	for _, c := range domain {
		if seen[c] {
			continue
		}
		siblings, err := readCPUList(filepath.Join(cpuDir(t.root, c), "topology", "thread_siblings_list"))
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
