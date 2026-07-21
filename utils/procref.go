package utils

import "fmt"

// ProcRef identifies one process generation: a pid plus its start time, so a
// recycled pid reads as a different process rather than the original one.
type ProcRef struct {
	PID   int
	Start uint64
}

// Valid reports whether the ref names an observed process.
func (p ProcRef) Valid() bool { return p.PID > 0 }

// ProcRefOf captures pid's current generation; where the start time is
// unreadable the pid alone identifies the process and reuse is undetectable.
func ProcRefOf(pid int) (ProcRef, error) {
	if pid <= 0 {
		return ProcRef{}, fmt.Errorf("invalid pid %d", pid)
	}
	start, err := procStartTime(pid)
	if err != nil {
		return ProcRef{}, err
	}
	return ProcRef{PID: pid, Start: start}, nil
}
