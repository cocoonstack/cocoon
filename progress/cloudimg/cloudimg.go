package cloudimg

const (
	PhaseDownload Phase = iota // HTTP download started.
	PhaseConvert               // Format conversion (qemu-img) started.
	PhaseCommit                // Writing to index.
	PhaseDone                  // Pull completed successfully.
)

// Phase represents a stage in the cloud image pull lifecycle.
type Phase int

// Event describes a single cloud image pull progress update.
type Event struct {
	Phase      Phase
	BytesTotal int64 // Content-Length; -1 if unknown.
	BytesDone  int64 // Bytes downloaded so far (download phase only).
}
