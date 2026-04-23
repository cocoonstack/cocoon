package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// defaultDiskQueueSize is the virtio-blk queue depth per device.
	defaultDiskQueueSize = 512
	cidataFile           = "cidata.img"
)

// kvBuilder accumulates key=value CLI fragments.
type kvBuilder []string

// DebugDiskCLIArgs uses the same storage-to-disk mapping as launch.
func DebugDiskCLIArgs(storageConfigs []*types.StorageConfig, cpuCount, diskQueueSize int, noDirectIO bool) []string {
	args := make([]string, 0, len(storageConfigs))
	for _, storageConfig := range storageConfigs {
		args = append(args, diskToCLIArg(storageConfigToDisk(storageConfig, cpuCount, diskQueueSize, noDirectIO)))
	}
	return args
}

// String joins all key=value pairs with commas.
func (b kvBuilder) String() string { return strings.Join(b, ",") }

func (b *kvBuilder) add(kv string) { *b = append(*b, kv) }
func (b *kvBuilder) addIf(cond bool, kv string) {
	if cond {
		*b = append(*b, kv)
	}
}

func buildVMConfig(_ context.Context, rec *hypervisor.VMRecord, consoleSockPath string) *chVMConfig {
	cpu := rec.Config.CPU
	mem := rec.Config.Memory

	maxVCPUs := runtime.NumCPU()

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

	// Disable balloon on Windows; the driver can spin during shutdown.
	if mem >= hypervisor.MinBalloonMemory && !rec.Config.Windows {
		cfg.Balloon = &chBalloon{
			Size:              mem / hypervisor.DefaultBalloonDiv, //nolint:mnd
			DeflateOnOOM:      true,
			FreePageReporting: true,
		}
	}

	for _, storageConfig := range activeDisks(rec) {
		cfg.Disks = append(cfg.Disks, storageConfigToDisk(storageConfig, cpu, rec.Config.DiskQueueSize, rec.Config.NoDirectIO))
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

// buildCLIArgs converts a chVMConfig into cloud-hypervisor CLI arguments.
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

func storageConfigToDisk(storageConfig *types.StorageConfig, cpuCount, diskQueueSize int, noDirectIO bool) chDisk {
	if diskQueueSize <= 0 {
		diskQueueSize = defaultDiskQueueSize
	}
	d := chDisk{
		Path:      storageConfig.Path,
		ReadOnly:  storageConfig.RO,
		Serial:    storageConfig.Serial,
		NumQueues: cpuCount,
		QueueSize: diskQueueSize,
	}

	d.DirectIO = !storageConfig.RO && !noDirectIO

	switch {
	case filepath.Ext(storageConfig.Path) == ".qcow2":
		d.ImageType = "Qcow2"
		d.BackingFiles = !storageConfig.RO
	case storageConfig.RO:
		d.ImageType = "Raw"
	default:
		d.ImageType = "Raw"
		d.Sparse = true
	}

	// Pin writable blk queues to vCPUs.
	if cpuCount > 1 && !storageConfig.RO {
		d.QueueAffinity = make([]chQueueAffinity, cpuCount)
		for i := range d.QueueAffinity {
			d.QueueAffinity[i] = chQueueAffinity{QueueIndex: i, HostCPUs: []int{i}}
		}
	}
	return d
}

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

// queueAffinityToCLI converts queue affinity to CH CLI format.
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

// isCidataDisk reports whether sc is the cloud-init disk.
func isCidataDisk(sc *types.StorageConfig) bool {
	return filepath.Base(sc.Path) == cidataFile
}

// activeDisks filters cidata out of post-first-boot cloudimg VMs.
func activeDisks(rec *hypervisor.VMRecord) []*types.StorageConfig {
	skipCidata := rec.FirstBooted && !isDirectBoot(rec.BootConfig)
	out := make([]*types.StorageConfig, 0, len(rec.StorageConfigs))
	for _, sc := range rec.StorageConfigs {
		if skipCidata && isCidataDisk(sc) {
			continue
		}
		out = append(out, sc)
	}
	return out
}
