// Package cgroup places VMM processes into per-VM cgroup v2 CPU scopes.
package cgroup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// Root is the cgroup v2 unified hierarchy mount point.
	Root = "/sys/fs/cgroup"
	// DefaultParent holds every per-VM scope unless cgroup_parent overrides it.
	DefaultParent = "cocoon.slice"
	// DefaultPeriodUs is the kernel's default cpu.max period.
	DefaultPeriodUs = 100000

	// MinWeight/MaxWeight are the kernel's cpu.weight bounds.
	MinWeight = 1
	MaxWeight = 10000
	// MinPeriodUs/MaxPeriodUs are the kernel's cpu.max period bounds.
	MinPeriodUs = 1000
	MaxPeriodUs = 1000000
	// MinQuotaUs is the kernel's minimum cpu.max quota.
	MinQuotaUs = 1000

	scopePrefix = "vm-"
	scopeSuffix = ".scope"

	subtreeControlName = "cgroup.subtree_control"
	killName           = "cgroup.kill"
	weightName         = "cpu.weight"
	maxName            = "cpu.max"
	burstName          = "cpu.max.burst"
	statName           = "cpu.stat"
	cpusetName         = "cpuset.cpus"
	cpusetEffName      = "cpuset.cpus.effective"

	removeWait         = time.Second
	removePollInterval = 10 * time.Millisecond
)

// Knobs is the resolved cgroup CPU configuration for one VM scope.
type Knobs struct {
	Weight   int
	QuotaUs  int64
	PeriodUs int64
	BurstUs  int64
	CPUSet   string
}

// ResolveKnobs applies the Guaranteed-at-N defaults: weight = vCPU count, quota = vCPU count x period, burst = 0, no placement.
func ResolveKnobs(cfg *types.Config) Knobs {
	period := cmp.Or(cfg.CPUPeriodUs, int64(DefaultPeriodUs))
	return Knobs{
		Weight:   cmp.Or(cfg.CPUWeight, cfg.CPU),
		QuotaUs:  cmp.Or(cfg.CPUQuotaUs, int64(cfg.CPU)*period),
		PeriodUs: period,
		BurstUs:  cfg.CPUBurstUs,
		CPUSet:   cfg.CPUSetCPUs,
	}
}

// Validate checks resolved knob values against the kernel's accepted ranges.
func (k Knobs) Validate() error {
	if k.Weight < MinWeight || k.Weight > MaxWeight {
		return fmt.Errorf("--cpu-weight must be %d..%d, got %d", MinWeight, MaxWeight, k.Weight)
	}
	if k.PeriodUs < MinPeriodUs || k.PeriodUs > MaxPeriodUs {
		return fmt.Errorf("--cpu-period-us must be %d..%d, got %d", MinPeriodUs, MaxPeriodUs, k.PeriodUs)
	}
	if k.QuotaUs < MinQuotaUs {
		return fmt.Errorf("--cpu-quota-us must be at least %d, got %d", MinQuotaUs, k.QuotaUs)
	}
	if k.BurstUs < 0 || k.BurstUs > k.QuotaUs {
		return fmt.Errorf("--cpu-burst-us must be 0..quota (%d), got %d", k.QuotaUs, k.BurstUs)
	}
	if _, err := ParseCPUList(k.CPUSet); err != nil {
		return fmt.Errorf("--cpuset-cpus: %w", err)
	}
	return nil
}

// EffectiveCPUs resolves the cpu set a VM may run on — explicit placement wins over the machine fence; nil means all cores.
func EffectiveCPUs(placement, fence string) []int {
	cpus, err := ParseCPUList(cmp.Or(placement, fence))
	if err != nil {
		return nil
	}
	return cpus
}

// ParseCPUList parses a kernel cpu-list ("0-14", "0,2-4"); empty input is nil (no placement).
func ParseCPUList(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var cpus []int
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, ok := strings.Cut(strings.TrimSpace(part), "-")
		start, err := strconv.Atoi(lo)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("invalid cpu list %q", s)
		}
		end := start
		if ok {
			if end, err = strconv.Atoi(hi); err != nil || end < start {
				return nil, fmt.Errorf("invalid cpu list %q", s)
			}
		}
		for c := start; c <= end; c++ {
			cpus = append(cpus, c)
		}
	}
	slices.Sort(cpus)
	return slices.Compact(cpus), nil
}

// ScopeDir returns vmID's scope directory under parentDir.
func ScopeDir(parentDir, vmID string) string {
	return filepath.Join(parentDir, scopePrefix+vmID+scopeSuffix)
}

// Prepare creates or reconfigures vmID's scope and returns its opened directory for CLONE_INTO_CGROUP; idempotent, so a relaunch reuses a scope its dying predecessor still occupies. deferQuota leaves the ceiling at max for the provisioning window — weight, fence, and placement still apply — until Arm sets the finite quota.
func Prepare(parentDir, fence, vmID string, k Knobs, deferQuota bool) (*os.File, error) {
	if vmID == "" {
		return nil, errors.New("cgroup scope: empty vm id")
	}
	if err := ensureParent(parentDir, fence, k.CPUSet != ""); err != nil {
		return nil, err
	}
	dir := ScopeDir(parentDir, vmID)
	mkErr := os.Mkdir(dir, 0o750)
	if mkErr != nil && !errors.Is(mkErr, fs.ErrExist) {
		return nil, fmt.Errorf("create scope: %w", mkErr)
	}
	if err := placeScope(parentDir, dir, k.CPUSet); err != nil {
		return nil, err
	}
	if err := writeControl(dir, weightName, strconv.Itoa(k.Weight)); err != nil {
		return nil, err
	}
	// A reused scope may hold a leftover burst > target quota, which blocks the cpu.max write (kernel requires burst <= quota): zero it first. ENOENT tolerated — pre-5.14 kernels lack the file.
	if errors.Is(mkErr, fs.ErrExist) {
		if err := writeControl(dir, burstName, "0"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if deferQuota {
		if err := writeControl(dir, maxName, fmt.Sprintf("max %d", k.PeriodUs)); err != nil {
			return nil, err
		}
	} else if err := armQuota(dir, k); err != nil {
		return nil, err
	}
	scope, err := os.Open(dir) //nolint:gosec // path derives from config parent + generated VM ID
	if err != nil {
		return nil, fmt.Errorf("open scope: %w", err)
	}
	return scope, nil
}

// Arm sets the finite quota on a scope prepared with deferQuota; called after memory load, before resume, so the guest never runs uncapped. Idempotent — a retry after a partial failure converges.
func Arm(parentDir, vmID string, k Knobs) error {
	return armQuota(ScopeDir(parentDir, vmID), k)
}

// Remove kills everything left in an owned scope and removes it; the VMM must already be confirmed dead. ENOENT counts as success.
func Remove(ctx context.Context, parentDir, vmID string) error {
	dir := ScopeDir(parentDir, vmID)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// Best-effort: catches the CH pty child and stray forks; an empty scope has nothing to kill.
	_ = writeControl(dir, killName, "1")
	// EBUSY polls: killed members need a moment to exit before rmdir succeeds.
	if err := utils.WaitFor(ctx, removeWait, removePollInterval, func() (bool, error) {
		switch rmErr := os.Remove(dir); {
		case rmErr == nil || errors.Is(rmErr, fs.ErrNotExist):
			return true, nil
		case errors.Is(rmErr, syscall.EBUSY):
			return false, nil
		default:
			return false, rmErr
		}
	}); err != nil {
		return fmt.Errorf("remove scope %s: %w", dir, err)
	}
	return nil
}

// RemoveEmpty removes vmID's scope only if empty; ENOENT counts as success. GC's variant for unowned scopes — it never kills.
func RemoveEmpty(parentDir, vmID string) error {
	err := os.Remove(ScopeDir(parentDir, vmID))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ListScopeVMIDs returns the VM IDs of all scopes under parentDir; a missing parent is empty.
func ListScopeVMIDs(parentDir string) ([]string, error) {
	names, err := utils.ScanSubdirs(parentDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, name := range names {
		if strings.HasPrefix(name, scopePrefix) && strings.HasSuffix(name, scopeSuffix) {
			ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(name, scopePrefix), scopeSuffix))
		}
	}
	return ids, nil
}

// ReadStat parses vmID's cpu.stat into key/value pairs.
func ReadStat(parentDir, vmID string) (map[string]int64, error) {
	data, err := os.ReadFile(filepath.Join(ScopeDir(parentDir, vmID), statName))
	if err != nil {
		return nil, err
	}
	return parseStat(string(data)), nil
}

func parseStat(data string) map[string]int64 {
	stat := make(map[string]int64)
	for line := range strings.Lines(data) {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			stat[key] = n
		}
	}
	return stat
}

// ensureParent enables the needed controllers at every ancestor, not just the leaf — cgroup v2 subtree delegation is hierarchical.
func ensureParent(parentDir, fence string, placed bool) error {
	rel, err := filepath.Rel(Root, parentDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("cgroup parent %q must be under %s", parentDir, Root)
	}
	if err := utils.EnsureDirs(parentDir); err != nil {
		return err
	}
	ctrls := []string{"cpu"}
	if fence != "" || placed {
		ctrls = append(ctrls, "cpuset")
	}
	if err := forEachLevel(rel, func(dir string) error { return enableControllers(dir, ctrls) }); err != nil {
		return err
	}
	return reconcileFence(parentDir, fence)
}

// reconcileFence converges the parent's cpuset.cpus to the configured fence; a cleared config resets a stale fence once, and the cpuset controller is never disabled.
func reconcileFence(parentDir, fence string) error {
	current, err := readControl(parentDir, cpusetName)
	if fence == "" {
		if err == nil && current != "" {
			return writeControl(parentDir, cpusetName, "\n")
		}
		return nil
	}
	// The kernel echoes cpu lists canonicalized ("0-3,4-7" reads back "0-7"): compare parsed sets, or a non-canonical config string re-runs the scan and the serialized cpuset write on every launch.
	if cur, curErr := ParseCPUList(current); curErr == nil {
		if want, wantErr := ParseCPUList(fence); wantErr == nil && slices.Equal(cur, want) {
			return nil
		}
	}
	if err := checkSubset(fence, filepath.Dir(parentDir), "cgroup_cpus fence"); err != nil {
		return err
	}
	if err := checkScopePlacements(parentDir, fence); err != nil {
		return err
	}
	return writeControl(parentDir, cpusetName, fence)
}

// placeScope applies a per-VM placement; the kernel silently degrades ungrantable requests to the parent set, so the subset check is cocoon's.
func placeScope(parentDir, dir, cpuset string) error {
	if cpuset == "" {
		return nil
	}
	if current, err := readControl(dir, cpusetName); err == nil {
		if cur, curErr := ParseCPUList(current); curErr == nil {
			if want, wantErr := ParseCPUList(cpuset); wantErr == nil && slices.Equal(cur, want) {
				return nil
			}
		}
	}
	if err := checkSubset(cpuset, parentDir, "--cpuset-cpus"); err != nil {
		return err
	}
	return writeControl(dir, cpusetName, cpuset)
}

func checkSubset(cpuset, dir, what string) error {
	want, err := ParseCPUList(cpuset)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	effRaw, err := readControl(dir, cpusetEffName)
	if err != nil {
		return fmt.Errorf("read effective cpuset: %w", err)
	}
	eff, err := ParseCPUList(effRaw)
	if err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Join(dir, cpusetEffName), err)
	}
	for _, c := range want {
		if !slices.Contains(eff, c) {
			return fmt.Errorf("%s %q: cpu %d not in %s effective set %q", what, cpuset, c, dir, effRaw)
		}
	}
	return nil
}

// checkScopePlacements refuses a fence shrink that would invalidate a live VM's explicit placement.
func checkScopePlacements(parentDir, fence string) error {
	ids, err := ListScopeVMIDs(parentDir)
	if err != nil {
		return err
	}
	fenceCPUs, _ := ParseCPUList(fence)
	for _, id := range ids {
		placement, err := readControl(ScopeDir(parentDir, id), cpusetName)
		if err != nil || placement == "" {
			continue
		}
		cpus, err := ParseCPUList(placement)
		if err != nil {
			continue
		}
		for _, c := range cpus {
			if !slices.Contains(fenceCPUs, c) {
				return fmt.Errorf("fence %q excludes cpu %d used by vm %s placement %q; stop that VM or widen the fence", fence, c, id, placement)
			}
		}
	}
	return nil
}

func forEachLevel(rel string, fn func(dir string) error) error {
	if err := fn(Root); err != nil {
		return err
	}
	dir := Root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		if err := fn(dir); err != nil {
			return err
		}
	}
	return nil
}

// enableControllers reads before writing: subtree_control writes take the kernel's hierarchy-wide cgroup_mutex, so steady-state launches must not contend on a no-op write. Missing controllers go in one combined write.
func enableControllers(dir string, ctrls []string) error {
	have, _ := readControl(dir, subtreeControlName)
	enabled := strings.Fields(have)
	var missing []string
	for _, ctrl := range ctrls {
		if !slices.Contains(enabled, ctrl) {
			missing = append(missing, "+"+ctrl)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return writeControl(dir, subtreeControlName, strings.Join(missing, " "))
}

func armQuota(dir string, k Knobs) error {
	if err := writeControl(dir, maxName, fmt.Sprintf("%d %d", k.QuotaUs, k.PeriodUs)); err != nil {
		return err
	}
	if k.BurstUs > 0 {
		if err := writeControl(dir, burstName, strconv.FormatInt(k.BurstUs, 10)); err != nil {
			return err
		}
	}
	return nil
}

func readControl(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixed name under the config-derived parent
	return strings.TrimSpace(string(data)), err
}

func writeControl(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
