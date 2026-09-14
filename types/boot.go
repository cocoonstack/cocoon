package types

// BootConfig holds kernel and firmware paths used to boot a VM.
type BootConfig struct {
	KernelPath string `json:"kernel_path,omitempty"`
	InitrdPath string `json:"initrd_path,omitempty"`
	Cmdline    string `json:"cmdline,omitempty"` // direct-boot kernel command line, set at Create from the storage layout

	FirmwarePath string `json:"firmware_path,omitempty"`
}
