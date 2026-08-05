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

	removeWait         = time.Second
	removePollInterval = 10 * time.Millisecond
)

// Knobs is the resolved cgroup CPU configuration for one VM scope.
type Knobs struct {
	Weight   int
	QuotaUs  int64
	PeriodUs int64
	BurstUs  int64
}

// ResolveKnobs applies the Guaranteed-at-N defaults: weight = vCPU count, quota = vCPU count x period, burst = 0.
func ResolveKnobs(cfg *types.Config) Knobs {
	period := cmp.Or(cfg.CPUPeriodUs, int64(DefaultPeriodUs))
	return Knobs{
		Weight:   cmp.Or(cfg.CPUWeight, cfg.CPU),
		QuotaUs:  cmp.Or(cfg.CPUQuotaUs, int64(cfg.CPU)*period),
		PeriodUs: period,
		BurstUs:  cfg.CPUBurstUs,
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
	return nil
}

// ScopeDir returns vmID's scope directory under parentDir.
func ScopeDir(parentDir, vmID string) string {
	return filepath.Join(parentDir, scopePrefix+vmID+scopeSuffix)
}

// Prepare creates or reconfigures vmID's scope and returns its opened directory for CLONE_INTO_CGROUP; idempotent, so a relaunch reuses a scope its dying predecessor still occupies.
func Prepare(parentDir, vmID string, k Knobs) (*os.File, error) {
	if vmID == "" {
		return nil, errors.New("cgroup scope: empty vm id")
	}
	if err := ensureParent(parentDir); err != nil {
		return nil, err
	}
	dir := ScopeDir(parentDir, vmID)
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create scope: %w", err)
	}
	if err := writeControl(dir, weightName, strconv.Itoa(k.Weight)); err != nil {
		return nil, err
	}
	// Reconfigure order: a leftover burst > target quota blocks the cpu.max write (kernel requires burst <= quota), so zero it first. ENOENT tolerated — pre-5.14 kernels lack the file.
	if err := writeControl(dir, burstName, "0"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := writeControl(dir, maxName, fmt.Sprintf("%d %d", k.QuotaUs, k.PeriodUs)); err != nil {
		return nil, err
	}
	if k.BurstUs > 0 {
		if err := writeControl(dir, burstName, strconv.FormatInt(k.BurstUs, 10)); err != nil {
			return nil, err
		}
	}
	scope, err := os.Open(dir) //nolint:gosec // path derives from config parent + generated VM ID
	if err != nil {
		return nil, fmt.Errorf("open scope: %w", err)
	}
	return scope, nil
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

// ensureParent creates parentDir and enables the cpu controller at every level from Root down; each write failure names the exact file, which is the platform preflight.
func ensureParent(parentDir string) error {
	rel, err := filepath.Rel(Root, parentDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("cgroup parent %q must be under %s", parentDir, Root)
	}
	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return fmt.Errorf("create cgroup parent: %w", err)
	}
	if err := enableCPU(Root); err != nil {
		return err
	}
	dir := Root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		if err := enableCPU(dir); err != nil {
			return err
		}
	}
	return nil
}

// enableCPU reads before writing: subtree_control writes take the kernel's hierarchy-wide cgroup_mutex, so steady-state launches must not contend on a no-op write.
func enableCPU(dir string) error {
	path := filepath.Join(dir, subtreeControlName)
	if data, err := os.ReadFile(path); err == nil && slices.Contains(strings.Fields(string(data)), "cpu") { //nolint:gosec // fixed name under the config-derived parent
		return nil
	}
	return writeControl(dir, subtreeControlName, "+cpu")
}

func writeControl(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
