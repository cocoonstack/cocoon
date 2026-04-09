package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// defaultDiskQueueSize is the virtio-blk queue depth per device.
	// 512 doubles the CH default (256) to allow more in-flight I/O,
	// improving random write throughput with moderate memory overhead.
	defaultDiskQueueSize = 512
	// defaultBalloon is the memory divisor for balloon sizing: mem/defaultBalloon
	// gives the initial balloon size (25% of total memory). The balloon starts
	// inflated to 75%, allowing OOM deflation headroom.
	defaultBalloon = 4
	cidataFile     = "cidata.img"
)

func buildVMConfig(ctx context.Context, rec *hypervisor.VMRecord, consoleSockPath string) *chVMConfig {
	cpu := rec.Config.CPU
	mem := rec.Config.Memory

	maxVCPUs := runtime.NumCPU()
	if cpu > maxVCPUs {
		log.WithFunc("cloudhypervisor.buildVMConfig").Warnf(ctx,
			"requested %d vCPUs exceeds host cores (%d), clamping to %d", cpu, maxVCPUs, maxVCPUs)
		cpu = maxVCPUs
	}

	cfg := &chVMConfig{
		CPUs:     chCPUs{BootVCPUs: cpu, MaxVCPUs: maxVCPUs, KVMHyperV: rec.Config.Windows},
		Memory:   chMemory{Size: mem, HugePages: utils.DetectHugePages()},
		RNG:      chRNG{Src: "/dev/urandom"},
		Watchdog: true,
	}

	if isDirectBoot(rec.BootConfig) {
		cfg.Serial = &chRuntimeFile{Mode: "Off"}
		cfg.Console = &chRuntimeFile{Mode: "Pty"}
	} else {
		cfg.Serial = &chRuntimeFile{Mode: "Socket", Socket: consoleSockPath}
		cfg.Console = &chRuntimeFile{Mode: "Off"}
	}

	// Balloon: 25% of memory, only when memory >= 256 MiB.
	// Disabled for Windows: virtio-win balloon driver >= 0.1.262 retries
	// deflation on host timeout (PR #1157) instead of giving up, causing
	// 100% CPU for minutes during shutdown and preventing ResetSystem from
	// reaching Cloud Hypervisor's ACPI shutdown port.
	if mem >= minBalloonMemory && !rec.Config.Windows {
		cfg.Balloon = &chBalloon{
			Size:              mem / defaultBalloon, //nolint:mnd
			DeflateOnOOM:      true,
			FreePageReporting: true,
		}
	}

	for _, storageConfig := range rec.StorageConfigs {
		if rec.FirstBooted && !isDirectBoot(rec.BootConfig) && isCidataDisk(storageConfig) {
			continue
		}
		cfg.Disks = append(cfg.Disks, storageConfigToDisk(storageConfig, cpu))
	}

	for _, nc := range rec.NetworkConfigs {
		cfg.Nets = append(cfg.Nets, networkConfigToNet(nc))
	}

	if boot := rec.BootConfig; boot != nil {
		switch {
		case boot.KernelPath != "":
			cfg.Payload = &chPayload{
				Kernel:    boot.KernelPath,
				Initramfs: boot.InitrdPath,
				Cmdline:   boot.Cmdline,
			}
		case boot.FirmwarePath != "":
			cfg.Payload = &chPayload{Firmware: boot.FirmwarePath}
		}
	}

	return cfg
}

func networkConfigToNet(nc *types.NetworkConfig) chNet {
	return chNet{
		Tap:         nc.Tap,
		Mac:         nc.Mac,
		NumQueues:   nc.NumQueues,
		QueueSize:   nc.QueueSize,
		OffloadTSO:  true,
		OffloadUFO:  true,
		OffloadCsum: true,
	}
}

func storageConfigToDisk(storageConfig *types.StorageConfig, cpuCount int) chDisk {
	d := chDisk{
		Path:      storageConfig.Path,
		ReadOnly:  storageConfig.RO,
		Serial:    storageConfig.Serial,
		NumQueues: cpuCount,
		QueueSize: defaultDiskQueueSize,
	}

	// Writable disks use O_DIRECT to bypass host page cache, avoiding
	// fdatasync storms on guest flush. Readonly disks keep page cache
	// for shared base image benefit (DirectIO defaults to false).
	d.DirectIO = !storageConfig.RO

	switch {
	case filepath.Ext(storageConfig.Path) == ".qcow2":
		// cloudimg qcow2 overlay: CH has its own L2/refcount LRU cache.
		d.ImageType = "Qcow2"
		d.BackingFiles = !storageConfig.RO
	case storageConfig.RO:
		// OCI EROFS layer: readonly, host page cache shared across VMs.
		d.ImageType = "Raw"
	default:
		// OCI COW raw: writable sparse disk.
		d.ImageType = "Raw"
		d.Sparse = true
	}

	// Bind each virtio-blk queue to its corresponding vCPU to reduce
	// cross-core cache bouncing on writable disks. Skip for readonly
	// disks where IO is low and fully served by page cache.
	if cpuCount > 1 && !storageConfig.RO {
		d.QueueAffinity = make([]chQueueAffinity, cpuCount)
		for i := range d.QueueAffinity {
			d.QueueAffinity[i] = chQueueAffinity{QueueIndex: i, HostCPUs: []int{i}}
		}
	}
	return d
}

// DebugDiskCLIArgs returns user-facing CH disk CLI args using the same
// storage-to-disk mapping as the runtime launch path.
func DebugDiskCLIArgs(storageConfigs []*types.StorageConfig, cpuCount int) []string {
	args := make([]string, 0, len(storageConfigs))
	for _, storageConfig := range storageConfigs {
		args = append(args, diskToCLIArg(storageConfigToDisk(storageConfig, cpuCount)))
	}
	return args
}

// buildCLIArgs converts a chVMConfig into cloud-hypervisor CLI arguments.
// The resulting args include --api-socket so the socket remains available
// for later control operations (stop, shutdown, power-button).
func buildCLIArgs(cfg *chVMConfig, socketPath string) []string {
	args := []string{"--api-socket", socketPath}

	var cpuKV kvBuilder
	cpuKV.add(fmt.Sprintf("boot=%d", cfg.CPUs.BootVCPUs))
	cpuKV.add(fmt.Sprintf("max=%d", cfg.CPUs.MaxVCPUs))
	cpuKV.addIf(cfg.CPUs.KVMHyperV, "kvm_hyperv=on")
	args = append(args, "--cpus", cpuKV.String())

	mem := fmt.Sprintf("size=%d", cfg.Memory.Size)
	if cfg.Memory.HugePages {
		mem += ",hugepages=on"
	}
	args = append(args, "--memory", mem)

	if len(cfg.Disks) > 0 {
		args = append(args, "--disk")
		for _, d := range cfg.Disks {
			args = append(args, diskToCLIArg(d))
		}
	}

	if p := cfg.Payload; p != nil {
		if p.Kernel != "" {
			args = append(args, "--kernel", p.Kernel)
		}
		if p.Firmware != "" {
			args = append(args, "--firmware", p.Firmware)
		}
		if p.Initramfs != "" {
			args = append(args, "--initramfs", p.Initramfs)
		}
		if p.Cmdline != "" {
			args = append(args, "--cmdline", p.Cmdline)
		}
	}

	if len(cfg.Nets) > 0 {
		args = append(args, "--net")
		for _, n := range cfg.Nets {
			args = append(args, netToCLIArg(n))
		}
	}

	args = append(args, "--rng", fmt.Sprintf("src=%s", cfg.RNG.Src))

	if cfg.Watchdog {
		args = append(args, "--watchdog")
	}

	if b := cfg.Balloon; b != nil {
		args = append(args, "--balloon", balloonToCLIArg(b))
	}

	if cfg.Serial != nil {
		args = append(args, "--serial", runtimeFiletoCLIArg(cfg.Serial))
	}
	if cfg.Console != nil {
		args = append(args, "--console", runtimeFiletoCLIArg(cfg.Console))
	}

	return args
}

// kvBuilder accumulates key=value pairs for CH CLI arguments.
type kvBuilder []string

func (b *kvBuilder) add(kv string) { *b = append(*b, kv) }
func (b *kvBuilder) addIf(cond bool, kv string) {
	if cond {
		*b = append(*b, kv)
	}
}
func (b kvBuilder) String() string { return strings.Join(b, ",") }

func diskToCLIArg(d chDisk) string {
	var b kvBuilder
	b.add("path=" + d.Path)
	b.addIf(d.ReadOnly, "readonly=on")
	b.addIf(d.DirectIO, "direct=on")
	b.addIf(d.Sparse, "sparse=on")
	b.addIf(d.ImageType != "", "image_type="+strings.ToLower(d.ImageType))
	b.addIf(d.BackingFiles, "backing_files=on")
	b.addIf(d.NumQueues > 0, fmt.Sprintf("num_queues=%d", d.NumQueues))
	b.addIf(d.QueueSize > 0, fmt.Sprintf("queue_size=%d", d.QueueSize))
	if len(d.QueueAffinity) > 0 {
		b.add("queue_affinity=" + queueAffinityToCLI(d.QueueAffinity))
	}
	b.addIf(d.Serial != "", "serial="+d.Serial)
	return b.String()
}

func netToCLIArg(n chNet) string {
	var b kvBuilder
	b.add("tap=" + n.Tap)
	b.addIf(n.Mac != "", "mac="+n.Mac)
	b.addIf(n.NumQueues > 0, fmt.Sprintf("num_queues=%d", n.NumQueues))
	b.addIf(n.QueueSize > 0, fmt.Sprintf("queue_size=%d", n.QueueSize))
	b.addIf(n.OffloadTSO, "offload_tso=on")
	b.addIf(n.OffloadUFO, "offload_ufo=on")
	b.addIf(n.OffloadCsum, "offload_csum=on")
	return b.String()
}

func balloonToCLIArg(b *chBalloon) string {
	var args kvBuilder
	args.add(fmt.Sprintf("size=%d", b.Size))
	args.addIf(b.DeflateOnOOM, "deflate_on_oom=on")
	args.addIf(b.FreePageReporting, "free_page_reporting=on")
	return args.String()
}

func runtimeFiletoCLIArg(c *chRuntimeFile) string {
	switch strings.ToLower(c.Mode) {
	case "file":
		return "file=" + c.File
	case "socket":
		return "socket=" + c.Socket
	case "tty":
		return "tty"
	default:
		return strings.ToLower(c.Mode) // "off", "null", "pty"
	}
}

// queueAffinityToCLI converts structured queue affinity to CH CLI format.
// e.g. []chQueueAffinity{{0,[0]},{1,[1]}} → "[0@[0],1@[1]]"
func queueAffinityToCLI(qa []chQueueAffinity) string {
	parts := make([]string, len(qa))
	for i, a := range qa {
		cpus := make([]string, len(a.HostCPUs))
		for j, c := range a.HostCPUs {
			cpus[j] = strconv.Itoa(c)
		}
		parts[i] = fmt.Sprintf("%d@[%s]", a.QueueIndex, strings.Join(cpus, ","))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// isCidataDisk reports whether a storage config is the cloud-init cidata disk.
func isCidataDisk(sc *types.StorageConfig) bool {
	return filepath.Base(sc.Path) == cidataFile
}
