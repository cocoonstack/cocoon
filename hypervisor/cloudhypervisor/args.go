package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

const defaultDiskQueueSize = 256

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
		CPUs:     chCPUs{BootVCPUs: cpu, MaxVCPUs: maxVCPUs},
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
	if mem >= minBalloonMemory {
		cfg.Balloon = &chBalloon{
			Size:              mem / 4, //nolint:mnd
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
		NumQueues:   nc.Queue,
		QueueSize:   nc.QueueSize,
		OffloadTSO:  true,
		OffloadUFO:  true,
		OffloadCsum: true,
	}
}

// netNumQueues returns the CH-level num_queues for a virtio-net device.
// Each vCPU gets its own TX/RX queue pair (1 TX + 1 RX = 2 virtio queues).
//   - cpu <= 1: 2 (single queue pair, tap opened with ONE_QUEUE)
//   - cpu >  1: cpu * 2 (multi-queue, tap opened with MULTI_QUEUE)
func netNumQueues(cpu int) int64 {
	if cpu <= 1 {
		return 2 //nolint:mnd
	}
	return int64(cpu) * 2 //nolint:mnd
}

// isCidataDisk reports whether a storage config is the cloud-init cidata disk.
func isCidataDisk(sc *types.StorageConfig) bool {
	return filepath.Base(sc.Path) == "cidata.img"
}

func storageConfigToDisk(storageConfig *types.StorageConfig, cpuCount int) chDisk {
	d := chDisk{
		Path:      storageConfig.Path,
		ReadOnly:  storageConfig.RO,
		Serial:    storageConfig.Serial,
		NumQueues: cpuCount,
		QueueSize: defaultDiskQueueSize,
	}

	switch {
	case filepath.Ext(storageConfig.Path) == ".qcow2":
		// cloudimg qcow2 overlay
		d.ImageType = "Qcow2"
		d.BackingFiles = !storageConfig.RO
	case storageConfig.RO:
		// OCI EROFS layer: readonly, direct I/O
		d.ImageType = "Raw"
		d.Direct = true
	default:
		// OCI COW raw: writable, direct I/O, sparse
		d.ImageType = "Raw"
		d.Direct = true
		d.Sparse = true
	}
	return d
}

// buildCLIArgs converts a chVMConfig into cloud-hypervisor CLI arguments.
// The resulting args include --api-socket so the socket remains available
// for later control operations (stop, shutdown, power-button).
func buildCLIArgs(cfg *chVMConfig, socketPath string) []string {
	args := []string{"--api-socket", socketPath}

	args = append(args, "--cpus", fmt.Sprintf("boot=%d,max=%d", cfg.CPUs.BootVCPUs, cfg.CPUs.MaxVCPUs))

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

func diskToCLIArg(d chDisk) string {
	parts := []string{"path=" + d.Path}
	if d.ReadOnly {
		parts = append(parts, "readonly=on")
	}
	if d.Direct {
		parts = append(parts, "direct=off")
	}
	if d.Sparse {
		parts = append(parts, "sparse=on")
	}
	if d.ImageType != "" {
		parts = append(parts, "image_type="+strings.ToLower(d.ImageType))
	}
	if d.BackingFiles {
		parts = append(parts, "backing_files=on")
	}
	if d.NumQueues > 0 {
		parts = append(parts, fmt.Sprintf("num_queues=%d", d.NumQueues))
	}
	if d.QueueSize > 0 {
		parts = append(parts, fmt.Sprintf("queue_size=%d", d.QueueSize))
	}
	if d.Serial != "" {
		parts = append(parts, "serial="+d.Serial)
	}
	return strings.Join(parts, ",")
}

func netToCLIArg(n chNet) string {
	parts := []string{"tap=" + n.Tap}
	if n.Mac != "" {
		parts = append(parts, "mac="+n.Mac)
	}
	if n.NumQueues > 0 {
		parts = append(parts, fmt.Sprintf("num_queues=%d", netNumQueues(int(n.NumQueues))))
	}
	if n.QueueSize > 0 {
		parts = append(parts, fmt.Sprintf("queue_size=%d", n.QueueSize))
	}
	if n.OffloadTSO {
		parts = append(parts, "offload_tso=on")
	}
	if n.OffloadUFO {
		parts = append(parts, "offload_ufo=on")
	}
	if n.OffloadCsum {
		parts = append(parts, "offload_csum=on")
	}
	return strings.Join(parts, ",")
}

func balloonToCLIArg(b *chBalloon) string {
	parts := []string{fmt.Sprintf("size=%d", b.Size)}
	if b.DeflateOnOOM {
		parts = append(parts, "deflate_on_oom=on")
	}
	if b.FreePageReporting {
		parts = append(parts, "free_page_reporting=on")
	}
	return strings.Join(parts, ",")
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
