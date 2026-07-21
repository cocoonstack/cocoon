package utils

// ProcRef is one process generation: pid plus start time, so a recycled pid reads as a different process.
type ProcRef struct {
	PID   int
	Start uint64
}

// ProcRefOf captures pid's current generation.
func ProcRefOf(pid int) (ProcRef, error) {
	start, err := procStartTime(pid)
	if err != nil {
		return ProcRef{}, err
	}
	return ProcRef{PID: pid, Start: start}, nil
}
