package oci

const (
	PhasePull Phase = iota
	PhaseLayer
	PhaseCommit
	PhaseDone
)

// Phase represents a stage in the OCI pull lifecycle.
type Phase int

// Event describes a single OCI pull progress update.
type Event struct {
	Phase  Phase
	Index  int // Layer index (0-based); -1 for non-layer phases.
	Total  int
	Digest string // Short digest hex (first 12 chars) for layer events.
}
