package oci

const (
	PhasePull   Phase = iota // Image resolved, layer count known.
	PhaseLayer               // A single layer has been processed.
	PhaseCommit              // Committing artifacts to shared image paths.
	PhaseDone                // Pull completed successfully.
)

// Phase represents a stage in the OCI pull lifecycle.
type Phase int

// Event describes a single OCI pull progress update.
type Event struct {
	Phase  Phase
	Index  int    // Layer index (0-based); -1 for non-layer phases.
	Total  int    // Total number of layers.
	Digest string // Short digest hex (first 12 chars) for layer events.
}
