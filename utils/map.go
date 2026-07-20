package utils

import (
	"maps"
)

// MergeSets unions any number of set maps into a new set.
func MergeSets[K comparable](sets ...map[K]struct{}) map[K]struct{} {
	total := 0
	for _, s := range sets {
		total += len(s)
	}
	out := make(map[K]struct{}, total)
	for _, s := range sets {
		maps.Copy(out, s)
	}
	return out
}

// MapValues projects every non-nil value in m through fn into a slice.
func MapValues[K comparable, V, R any](m map[K]*V, fn func(*V) R) []R {
	if len(m) == 0 {
		return nil
	}
	out := make([]R, 0, len(m))
	for _, v := range m {
		if v == nil {
			continue
		}
		out = append(out, fn(v))
	}
	return out
}
