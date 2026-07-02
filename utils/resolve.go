package utils

import (
	"fmt"
	"strings"
)

// ResolveRef resolves a ref (exact ID, name, or ID prefix ≥3 chars) to a full ID.
func ResolveRef[T any](items map[string]*T, names map[string]string, ref string, notFound error) (string, error) {
	if items[ref] != nil {
		return ref, nil
	}
	if id, ok := names[ref]; ok && items[id] != nil {
		return id, nil
	}
	if len(ref) >= 3 {
		var match string
		for id, item := range items {
			if item != nil && strings.HasPrefix(id, ref) {
				if match != "" {
					return "", fmt.Errorf("ambiguous ref %q: multiple matches", ref)
				}
				match = id
			}
		}
		if match != "" {
			return match, nil
		}
	}
	return "", notFound
}

// ResolveRefs batch-resolves refs to exact IDs, deduplicating results.
func ResolveRefs[T any](items map[string]*T, names map[string]string, refs []string, notFound error) ([]string, error) {
	seen := make(map[string]struct{}, len(refs))
	var ids []string
	for _, ref := range refs {
		id, err := ResolveRef(items, names, ref, notFound)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", ref, err)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// InitNamedIndex initializes nil maps in a named index (Items + Names pattern).
func InitNamedIndex[T any](items *map[string]*T, names *map[string]string) {
	if *items == nil {
		*items = make(map[string]*T)
	}
	if *names == nil {
		*names = make(map[string]string)
	}
}
