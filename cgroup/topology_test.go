package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestReadTopologyGroupsByLastLevelCache(t *testing.T) {
	root := writeSysfsCPUs(t, 2, 2, 2)
	topo, err := ReadTopology(root, nil)
	if err != nil {
		t.Fatalf("ReadTopology: %v", err)
	}
	want := [][]int{{0, 1, 4, 5}, {2, 3, 6, 7}}
	if !slices.EqualFunc(topo.domains, want, slices.Equal) {
		t.Errorf("domains = %v, want %v", topo.domains, want)
	}
}

func TestReadTopologyFence(t *testing.T) {
	root := writeSysfsCPUs(t, 2, 2, 2)
	topo, err := ReadTopology(root, []int{2, 3, 6})
	if err != nil {
		t.Fatalf("ReadTopology: %v", err)
	}
	if want := [][]int{{2, 3, 6}}; !slices.EqualFunc(topo.domains, want, slices.Equal) {
		t.Errorf("domains = %v, want %v", topo.domains, want)
	}
	if _, err := ReadTopology(root, []int{40}); err == nil {
		t.Error("fence outside the online set: want error")
	}
}

func TestReadTopologyWithoutCacheSysfs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "online"), []byte("0-3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	topo, err := ReadTopology(root, nil)
	if err != nil {
		t.Fatalf("ReadTopology: %v", err)
	}
	if want := [][]int{{0, 1, 2, 3}}; !slices.EqualFunc(topo.domains, want, slices.Equal) {
		t.Errorf("domains = %v, want %v", topo.domains, want)
	}
	_, pins, err := topo.Place(3, nil)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if want := []int{0, 1, 2}; !slices.Equal(pins, want) {
		t.Errorf("pins = %v, want %v", pins, want)
	}
}

func TestPlace(t *testing.T) {
	root := writeSysfsCPUs(t, 2, 2, 2)
	topo, err := ReadTopology(root, nil)
	if err != nil {
		t.Fatalf("ReadTopology: %v", err)
	}
	tests := []struct {
		name       string
		queues     int
		load       map[int]int
		wantDomain []int
		wantPins   []int
	}{
		{name: "empty host takes the first domain, one thread per core", queues: 2, wantDomain: []int{0, 1, 4, 5}, wantPins: []int{0, 1}},
		{name: "a loaded first domain yields to the second", queues: 2, load: map[int]int{0: 1, 1: 1}, wantDomain: []int{2, 3, 6, 7}, wantPins: []int{2, 3}},
		{name: "equal load ties to the lowest domain and the idle siblings", queues: 2, load: map[int]int{0: 1, 1: 1, 2: 1, 3: 1}, wantDomain: []int{0, 1, 4, 5}, wantPins: []int{4, 5}},
		{name: "more queues than cores spill onto siblings", queues: 3, wantDomain: []int{0, 1, 4, 5}, wantPins: []int{0, 1, 4}},
		{name: "more queues than threads pin fewer cpus than queues", queues: 6, wantDomain: []int{0, 1, 4, 5}, wantPins: []int{0, 1, 4, 5}},
		{name: "a single queue is never pinned", queues: 1, wantDomain: []int{0, 1, 4, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			domain, pins, err := topo.Place(tt.queues, tt.load)
			if err != nil {
				t.Fatalf("Place: %v", err)
			}
			if !slices.Equal(domain, tt.wantDomain) || !slices.Equal(pins, tt.wantPins) {
				t.Errorf("got domain %v pins %v, want domain %v pins %v", domain, pins, tt.wantDomain, tt.wantPins)
			}
		})
	}
}

func TestFormatCPUList(t *testing.T) {
	tests := []struct {
		cpus []int
		want string
	}{
		{cpus: nil, want: ""},
		{cpus: []int{3}, want: "3"},
		{cpus: []int{0, 1, 2, 3, 192, 193, 194, 195}, want: "0-3,192-195"},
		{cpus: []int{0, 2, 3, 7}, want: "0,2-3,7"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := FormatCPUList(tt.cpus); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			back, err := ParseCPUList(tt.want)
			if err != nil || !slices.Equal(back, tt.cpus) {
				t.Errorf("round trip = %v, %v", back, err)
			}
		})
	}
}

func writeSysfsCPUs(t *testing.T, domains, cores, threads int) string {
	t.Helper()
	root := t.TempDir()
	physical := domains * cores
	if err := os.WriteFile(filepath.Join(root, "online"), fmt.Appendf(nil, "0-%d\n", physical*threads-1), 0o600); err != nil {
		t.Fatal(err)
	}
	cpuOf := func(domain, core, thread int) int { return thread*physical + domain*cores + core }
	for domain := range domains {
		var shared []int
		for core := range cores {
			for thread := range threads {
				shared = append(shared, cpuOf(domain, core, thread))
			}
		}
		slices.Sort(shared)
		for core := range cores {
			var siblings []int
			for thread := range threads {
				siblings = append(siblings, cpuOf(domain, core, thread))
			}
			for _, cpu := range siblings {
				writeSysfsCPU(t, cpuDir(root, cpu), map[string]string{
					"cache/index0/level":            "1",
					"cache/index0/shared_cpu_list":  FormatCPUList(siblings),
					"cache/index2/level":            "3",
					"cache/index2/shared_cpu_list":  FormatCPUList(shared),
					"topology/thread_siblings_list": FormatCPUList(siblings),
				})
			}
		}
	}
	return root
}

func writeSysfsCPU(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
