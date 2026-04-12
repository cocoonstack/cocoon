package types

// StorageConfig describes a disk attached to a VM.
type StorageConfig struct {
	Path   string `json:"path"`
	RO     bool   `json:"ro"`
	Serial string `json:"serial"`
}
