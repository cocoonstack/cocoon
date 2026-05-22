package config

// MeteringConfig selects the recorder backend; empty Backend defaults to "file" so v0.4.x configs keep working.
type MeteringConfig struct {
	Backend string             `json:"backend,omitempty" mapstructure:"backend"`
	File    FileMeteringConfig `json:"file,omitzero"     mapstructure:"file"`
}

// FileMeteringConfig parameterizes the file-backend recorder; empty Path resolves to <RootDir>/metering/ledger.jsonl.
type FileMeteringConfig struct {
	Path string `json:"path,omitempty" mapstructure:"path"`
}
