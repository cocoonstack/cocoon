package types

// BootConfig holds kernel and firmware paths used to boot a VM.
type BootConfig struct {
	// Direct-boot fields (OCI images).
	KernelPath string `json:"kernel_path,omitempty"`
	InitrdPath string `json:"initrd_path,omitempty"`
	// Cmdline is the kernel command line for direct-boot VMs.
	// Set at Create time from the storage layout (cocoon.layers=, cocoon.cow=, …).
	Cmdline string `json:"cmdline,omitempty"`

	// UEFI-boot field (cloud images).
	FirmwarePath string `json:"firmware_path,omitempty"`
}
