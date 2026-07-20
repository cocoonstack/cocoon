package utils

import (
	"testing"
)

func TestMergeSets_Basic(t *testing.T) {
	s1 := map[string]struct{}{"a": {}, "b": {}}
	s2 := map[string]struct{}{"b": {}, "c": {}}

	got := MergeSets(s1, s2)
	if len(got) != 3 {
		t.Errorf("got %d items, want 3", len(got))
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
}

func TestMergeSets_Empty(t *testing.T) {
	got := MergeSets[string]()
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestMergeSets_NilSets(t *testing.T) {
	var s1 map[string]struct{}
	got := MergeSets(s1, nil)
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

func TestMergeSets_Single(t *testing.T) {
	s := map[int]struct{}{1: {}, 2: {}}
	got := MergeSets(s)
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestMergeSets_DoesNotModifyInput(t *testing.T) {
	s1 := map[string]struct{}{"a": {}}
	s2 := map[string]struct{}{"b": {}}

	_ = MergeSets(s1, s2)

	if len(s1) != 1 {
		t.Error("s1 was modified")
	}
	if len(s2) != 1 {
		t.Error("s2 was modified")
	}
}

func TestMapValues_Basic(t *testing.T) {
	type rec struct{ N int }
	m := map[string]*rec{"a": {N: 1}, "b": {N: 2}, "c": {N: 3}}

	got := MapValues(m, func(r *rec) int { return r.N })
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	sum := 0
	for _, v := range got {
		sum += v
	}
	if sum != 6 {
		t.Errorf("sum %d, want 6", sum)
	}
}

func TestMapValues_SkipsNil(t *testing.T) {
	type rec struct{ N int }
	m := map[string]*rec{"a": {N: 1}, "b": nil, "c": {N: 3}}

	got := MapValues(m, func(r *rec) int { return r.N })
	if len(got) != 2 {
		t.Errorf("got %d items, want 2 (nil should be skipped)", len(got))
	}
}

func TestMapValues_Empty(t *testing.T) {
	got := MapValues(map[string]*int{}, func(*int) int { return 0 })
	if got != nil {
		t.Errorf("expected nil for empty map, got %v", got)
	}
}

func TestMapValues_Nil(t *testing.T) {
	var m map[string]*int
	got := MapValues(m, func(*int) int { return 0 })
	if got != nil {
		t.Errorf("expected nil for nil map, got %v", got)
	}
}
