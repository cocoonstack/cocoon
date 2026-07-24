package config

import "fmt"

const (
	MeteringFile   MeteringBackend = "file"
	MeteringNop    MeteringBackend = "nop"
	MeteringStderr MeteringBackend = "stderr"
	MeteringMeta   MeteringBackend = "meta"
)

// MeteringBackend identifies the lifecycle-event recorder backend.
type MeteringBackend string

// FileMeteringConfig parameterizes the file-backend recorder; empty Path resolves to <RootDir>/metering/ledger.jsonl.
type FileMeteringConfig struct {
	Path string `json:"path,omitempty" mapstructure:"path"`
}

// MeteringConfig selects the recorder backend; empty Backend defaults to MeteringFile.
type MeteringConfig struct {
	Backend MeteringBackend    `json:"backend,omitempty" mapstructure:"backend"`
	File    FileMeteringConfig `json:"file,omitzero"     mapstructure:"file"`
}

// Validate rejects an unknown metering backend at startup.
func (m MeteringConfig) Validate() error {
	switch m.Backend {
	case "", MeteringFile, MeteringNop, MeteringStderr, MeteringMeta:
		return nil
	default:
		return fmt.Errorf("metering.backend %q is not one of file|nop|stderr|meta", m.Backend)
	}
}
