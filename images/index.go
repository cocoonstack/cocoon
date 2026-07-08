package images

import (
	"context"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const minHexLen = 12

// Entry is the common index-entry contract; backends implement it with value receivers.
type Entry interface {
	EntryID() string
	EntryRef() string
	EntryCreatedAt() time.Time
	DigestHexes() []string
}

// Index is the generic base both backends embed for Init and the Images map.
type Index[E any] struct {
	Images map[string]*E `json:"images"`
}

// Init implements storage.Initer. Called automatically by storejson.Store after loading.
func (idx *Index[E]) Init() {
	if idx.Images == nil {
		idx.Images = make(map[string]*E)
	}
}

// ReferencedDigests collects all blob digest hex strings referenced by any (*entry).
func ReferencedDigests[E Entry](images map[string]*E) map[string]struct{} {
	refs := make(map[string]struct{})
	for _, ep := range images {
		if ep == nil {
			continue
		}
		e := *ep
		for _, hex := range e.DigestHexes() {
			refs[hex] = struct{}{}
		}
	}
	return refs
}

// LookupOne resolves id to a single entry via LookupRefs' rules, so backend Config
// and the shared Inspect share one notion of "found" (including digest prefixes).
// Multiple refs are fine only while they name the same image (tag aliases of one
// digest); a prefix spanning distinct digests is ambiguous and resolves to nothing
// rather than to map-iteration luck.
func LookupOne[E Entry](images map[string]*E, id string, normalizers ...func(string) (string, bool)) (string, *E, bool) {
	refs := LookupRefs(images, id, normalizers...)
	if len(refs) == 0 {
		return "", nil, false
	}
	first := images[refs[0]]
	for _, ref := range refs[1:] {
		e := images[ref]
		if e == nil || (*e).EntryID() != (*first).EntryID() {
			return "", nil, false
		}
	}
	return refs[0], first, true
}

// LookupRefs returns all ref keys matching id by exact key, normalizer, or digest prefix.
func LookupRefs[E Entry](images map[string]*E, id string, normalizers ...func(string) (string, bool)) []string {
	if entry, ok := images[id]; ok && entry != nil {
		return []string{id}
	}
	// Try normalizers (e.g., OCI "ubuntu:24.04" -> "docker.io/library/ubuntu:24.04").
	for _, norm := range normalizers {
		if normalized, ok := norm(id); ok {
			if entry, ok := images[normalized]; ok && entry != nil {
				return []string{normalized}
			}
		}
	}
	// Digest match (exact or prefix); minHexLen guards against over-broad prefixes like "sha256:a".
	idHex := strings.TrimPrefix(id, "sha256:")
	var refs []string
	for ref, ep := range images {
		if ep == nil {
			continue
		}
		e := *ep
		dStr := e.EntryID()
		dHex := strings.TrimPrefix(dStr, "sha256:")
		if dStr == id || dHex == id {
			refs = append(refs, ref)
			continue
		}
		if len(idHex) >= minHexLen && strings.HasPrefix(dHex, idHex) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// deleteByID removes every ref returned by lookup, so "delete <digest>" sweeps all refs pointing at it.
func deleteByID[E any](ctx context.Context, logPrefix string, images map[string]*E, lookup func(string) []string, ids []string) []string {
	logger := log.WithFunc(logPrefix)
	var deleted []string
	for _, id := range ids {
		refs := lookup(id)
		if len(refs) == 0 {
			logger.Debugf(ctx, "image %q not found, skipping", id)
			continue
		}
		for _, ref := range refs {
			delete(images, ref)
			deleted = append(deleted, ref)
			logger.Debugf(ctx, "deleted from index: %s", ref)
		}
	}
	return deleted
}

// entryToImage converts a single index entry to *types.Image.
func entryToImage[E Entry](entry *E, typ string, sizer func(*E) int64) *types.Image {
	if entry == nil {
		return nil
	}
	e := *entry // value copy — detached from the index map
	return &types.Image{
		ID:        e.EntryID(),
		Name:      e.EntryRef(),
		Type:      typ,
		Size:      sizer(&e),
		CreatedAt: e.EntryCreatedAt(),
	}
}

// listImages iterates the index and builds a list of types.Image.
func listImages[E Entry](images map[string]*E, typ string, sizer func(*E) int64) []*types.Image {
	return utils.MapValues(images, func(ep *E) *types.Image {
		return entryToImage(ep, typ, sizer)
	})
}
