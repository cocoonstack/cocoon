package config

import "fmt"

const (
	MeteringFile   MeteringBackend = "file"
	MeteringNop    MeteringBackend = "nop"
	MeteringStderr MeteringBackend = "stderr"
)

// MeteringBackend identifies the lifecycle-event recorder backend.
type MeteringBackend string

// MeteringConfig selects the recorder backend; empty Backend defaults to MeteringFile so v0.4.x configs keep working.
type MeteringConfig struct {
	Backend MeteringBackend    `json:"backend,omitempty" mapstructure:"backend"`
	File    FileMeteringConfig `json:"file,omitzero"     mapstructure:"file"`
}

// FileMeteringConfig parameterizes the file-backend recorder; empty Path resolves to <RootDir>/metering/ledger.jsonl.
type FileMeteringConfig struct {
	Path string `json:"path,omitempty" mapstructure:"path"`
}

func (m MeteringConfig) Validate() error {
	switch m.Backend {
	case "", MeteringFile, MeteringNop, MeteringStderr:
		return nil
	default:
		return fmt.Errorf("metering.backend %q is not one of file|nop|stderr", m.Backend)
	}
}
