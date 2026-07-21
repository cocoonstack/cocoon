package utils

import (
	"os"
	"testing"
)

func TestProcRefOfSelfIsStable(t *testing.T) {
	pid := os.Getpid()
	first, err := ProcRefOf(pid)
	if err != nil {
		t.Fatalf("ProcRefOf: %v", err)
	}
	if first.PID != pid || !first.Valid() {
		t.Fatalf("got %+v, want a valid ref for pid %d", first, pid)
	}
	second, err := ProcRefOf(pid)
	if err != nil {
		t.Fatalf("ProcRefOf again: %v", err)
	}
	if first != second {
		t.Errorf("got %+v then %+v, want one generation to read identically", first, second)
	}
}

func TestProcRefOfRejectsInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if ref, err := ProcRefOf(pid); err == nil {
			t.Errorf("pid %d: got %+v, want an error", pid, ref)
		}
	}
}

func TestProcRefZeroValueIsNotValid(t *testing.T) {
	if (ProcRef{}).Valid() {
		t.Error("the zero ProcRef reported valid")
	}
}
