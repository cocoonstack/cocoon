// Package cloudimg defines the progress events of a cloud image pull.
package cloudimg

const (
	PhaseDownload Phase = iota
	PhaseConvert
	PhaseCommit
	PhaseDone
)

// Phase represents a stage in the cloud image pull lifecycle.
type Phase int

// Event describes a single cloud image pull progress update.
type Event struct {
	Phase      Phase
	BytesTotal int64 // from Content-Length; -1 if unknown
	BytesDone  int64 // bytes downloaded so far (download phase only)
}
