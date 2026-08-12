package cloudhypervisor

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	defaultDiskQueueSize = 512
	cidataFile           = "cidata.img"

	cocoonNetIDPrefix = "cocoon-net-"
)

type kvBuilder []string

func (b kvBuilder) String() string { return strings.Join(b, ",") }

func (b *kvBuilder) add(kv string) { *b = append(*b, kv) }
func (b *kvBuilder) addIf(cond bool, kv string) {
	if cond {
		*b = append(*b, kv)
	}
}

// DebugDiskCLIArgs uses the same storage-to-disk mapping as launch.
func DebugDiskCLIArgs(storageConfigs []*types.StorageConfig, cpuCount, diskQueueSize int, noDirectIO bool, allowed []int) []string {
	args := make([]string, 0, len(storageConfigs))
	for _, storageConfig := range storageConfigs {
		args = append(args, diskToCLIArg(storageConfigToDisk(storageConfig, cpuCount, diskQueueSize, noDirectIO, allowed)))
	}
	return args
}

// DebugMemoryCLIArg uses the same memory mapping as launch.
func DebugMemoryCLIArg(cfg *types.Config) string {
	return memoryCLIArg(chMemory{Size: cfg.Memory, HugePages: cfg.HugePages, Shared: cfg.SharedMemory, Mergeable: cfg.Mergeable})
}

func buildVMConfig(rec *hypervisor.VMRecord, consoleSockPath string, allowed []int) *chVMConfig {
	cpu := rec.Config.CPU
	mem := rec.Config.Memory

	cfg := &chVMConfig{
		CPUs:     chCPUs{BootVCPUs: cpu, MaxVCPUs: hypervisor.HostCPUCount(), KVMHyperV: rec.Config.Windows},
		Memory:   chMemory{Size: mem, HugePages: rec.Config.HugePages, Shared: rec.Config.SharedMemory, Mergeable: rec.Config.Mergeable},
		RNG:      chRNG{Src: "/dev/urandom"},
		Watchdog: true,
		Vsock:    &chVsock{CID: hypervisor.VsockGuestCID, Socket: hypervisor.VsockSockPath(rec.RunDir)},
	}

	cfg.Serial, cfg.Console = serialConsoleFor(hypervisor.IsDirectBoot(rec.BootConfig), consoleSockPath)

	if size, ok := hypervisor.BalloonSize(mem, rec.Config.Windows); ok {
		cfg.Balloon = &chBalloon{
			Size:              size,
			DeflateOnOOM:      true,
			FreePageReporting: true,
		}
	}

	for _, storageConfig := range activeDisks(rec) {
		cfg.Disks = append(cfg.Disks, storageConfigToDisk(storageConfig, cpu, rec.Config.DiskQueueSize, rec.Config.NoDirectIO, allowed))
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

func buildCLIArgs(cfg *chVMConfig, socketPath string) []string {
	args := []string{apiSocketFlag, socketPath}

	var cpuKV kvBuilder
	cpuKV.add(fmt.Sprintf("boot=%d", cfg.CPUs.BootVCPUs))
	cpuKV.add(fmt.Sprintf("max=%d", cfg.CPUs.MaxVCPUs))
	cpuKV.addIf(cfg.CPUs.KVMHyperV, "kvm_hyperv=on")
	args = append(args, "--cpus", cpuKV.String())

	args = append(args, "--memory", memoryCLIArg(cfg.Memory))

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

	if v := cfg.Vsock; v != nil {
		args = append(args, "--vsock", fmt.Sprintf("cid=%d,socket=%s", v.CID, v.Socket))
	}

	if cfg.Serial != nil {
		args = append(args, "--serial", runtimeFileToCLIArg(cfg.Serial))
	}
	if cfg.Console != nil {
		args = append(args, "--console", runtimeFileToCLIArg(cfg.Console))
	}

	return args
}

func networkConfigToNet(nc *types.NetworkConfig) chNet {
	return chNet{
		TAP:         nc.TAP,
		MAC:         nc.MAC,
		NumQueues:   nc.NumQueues,
		QueueSize:   nc.QueueSize,
		OffloadTSO:  true,
		OffloadUFO:  true,
		OffloadCsum: true,
	}
}

// cocoonNetID is the deterministic CH device id for a cocoon-managed NIC.
func cocoonNetID(mac string) string {
	return cocoonNetIDPrefix + strings.ReplaceAll(mac, ":", "")
}

// Launch args and restore-time config.json patching must agree on this rule.
func serialConsoleFor(directBoot bool, consoleSock string) (serial, console *chRuntimeFile) {
	if directBoot {
		return &chRuntimeFile{Mode: "Off"}, &chRuntimeFile{Mode: "Pty"}
	}
	return &chRuntimeFile{Mode: "Socket", Socket: consoleSock}, &chRuntimeFile{Mode: "Off"}
}

func qcow2Overlay(sc *types.StorageConfig) bool {
	return !sc.RO && filepath.Ext(sc.Path) == ".qcow2"
}

func effectiveDirectIO(sc *types.StorageConfig, noDirectIO bool) bool {
	if sc.DirectIO != nil {
		return *sc.DirectIO
	}
	// CH applies this flag to the backing file too, so O_DIRECT would stop one page-cache copy of the shared base serving every VM.
	if qcow2Overlay(sc) {
		return false
	}
	return !sc.RO && !noDirectIO
}

func storageConfigToDisk(storageConfig *types.StorageConfig, cpuCount, diskQueueSize int, noDirectIO bool, allowed []int) chDisk {
	diskQueueSize = utils.OrDefault(diskQueueSize, defaultDiskQueueSize)
	d := chDisk{
		Path:      storageConfig.Path,
		ReadOnly:  storageConfig.RO,
		Serial:    storageConfig.Serial,
		NumQueues: cpuCount,
		QueueSize: diskQueueSize,
		DirectIO:  effectiveDirectIO(storageConfig, noDirectIO),
	}

	switch {
	case filepath.Ext(storageConfig.Path) == ".qcow2":
		d.ImageType = "Qcow2"
		d.BackingFiles = qcow2Overlay(storageConfig)
	case storageConfig.RO:
		d.ImageType = "Raw"
	default:
		d.ImageType = "Raw"
		d.Sparse = true
	}

	if cpuCount > 1 && !storageConfig.RO {
		d.QueueAffinity = queueAffinity(cpuCount, allowed)
	}
	return d
}

// queueAffinity spreads queue i over host CPUs, clamped to the allowed set (fence/placement) so no target lands on a core the scope cannot run on; nil allowed keeps the identity mapping.
func queueAffinity(cpuCount int, allowed []int) []chQueueAffinity {
	qa := make([]chQueueAffinity, cpuCount)
	for i := range qa {
		host := i
		if len(allowed) > 0 {
			host = allowed[i%len(allowed)]
		}
		qa[i] = chQueueAffinity{QueueIndex: i, HostCPUs: []int{host}}
	}
	return qa
}

func memoryCLIArg(m chMemory) string {
	var kv kvBuilder
	kv.add(fmt.Sprintf("size=%d", m.Size))
	kv.addIf(m.HugePages, "hugepages=on")
	kv.addIf(m.Shared, "shared=on")
	kv.addIf(m.Mergeable, "mergeable=on")
	return kv.String()
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
	b.add("tap=" + n.TAP)
	b.addIf(n.MAC != "", "mac="+n.MAC)
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

func runtimeFileToCLIArg(c *chRuntimeFile) string {
	switch strings.ToLower(c.Mode) {
	case "file":
		return "file=" + c.File
	case "socket":
		return "socket=" + c.Socket
	default:
		return strings.ToLower(c.Mode) // "off", "null", "tty", "pty"
	}
}

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

// activeDisks filters cidata out of post-first-boot cloudimg VMs.
func activeDisks(rec *hypervisor.VMRecord) []*types.StorageConfig {
	skipCidata := rec.FirstBooted && !hypervisor.IsDirectBoot(rec.BootConfig)
	out := make([]*types.StorageConfig, 0, len(rec.StorageConfigs))
	for _, sc := range rec.StorageConfigs {
		if skipCidata && sc.Role == types.StorageRoleCidata {
			continue
		}
		out = append(out, sc)
	}
	return out
}
