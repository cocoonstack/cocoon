package images

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const minHexLen = 12

// Entry defines the common behavior of an image index (*entry).
// Both OCI and cloudimg imageEntry types implement this with value receivers.
type Entry interface {
	EntryID() string
	EntryRef() string
	EntryCreatedAt() time.Time
	DigestHexes() []string
}

// Index is the shared generic base for image indices.
// Both backends embed Index[imageEntry] to inherit Init() and the Images map.
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

// LookupRefs returns all ref keys matching id by exact key, optional
// normalization, or digest prefix. normalizers are tried in order for
// backend-specific key transforms (e.g., OCI image reference normalization).
func LookupRefs[E Entry](images map[string]*E, id string, normalizers ...func(string) (string, bool)) []string {
	// Exact key match.
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
	// Digest match (exact or prefix) — collect ALL matching refs.
	// Require at least minHexLen hex characters for prefix match to avoid
	// overly broad matches (e.g., "sha256:a" hitting everything).
	// Strip optional "sha256:" before measuring so the threshold counts
	// actual hex digits, not the algorithm prefix.
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
		if len(idHex) >= minHexLen && (strings.HasPrefix(dHex, idHex)) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// deleteByID removes entries from the map by looking up each ID.
// lookup returns all matching ref keys (supporting digest prefix and multi-ref
// matches), so "delete <digest>" removes every ref pointing to that digest.
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
	var result []*types.Image
	for _, ep := range images {
		if img := entryToImage(ep, typ, sizer); img != nil {
			result = append(result, img)
		}
	}
	return result
}

// GCStaleTemp removes temp entries older than StaleTempAge.
// Set dirOnly=true to only remove directories (OCI uses dirs, cloudimg uses files).
//
// Regular files whose name ends in ".lock" are preserved regardless of
// age — they are flock rendezvous files, and removing one while another
// process is holding the lock would break cross-process mutual
// exclusion (flock synchronizes on inode, not pathname: a subsequent
// flock.New on the same path would create a new inode and race with
// the lock holder). They are 0-byte files so the leak is negligible.
func GCStaleTemp(ctx context.Context, dir string, dirOnly bool) []error {
	cutoff := time.Now().Add(-utils.StaleTempAge)
	return utils.RemoveMatching(ctx, dir, func(e os.DirEntry) bool {
		if dirOnly && !e.IsDir() {
			return false
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
			return false
		}
		info, err := e.Info()
		return err == nil && info.ModTime().Before(cutoff)
	})
}

// GCCollectBlobs removes temp files and blob artifacts by hex ID.
// removers are called for each hex; os.IsNotExist errors are ignored.
func GCCollectBlobs(ctx context.Context, tempDir string, dirOnly bool, ids []string, removers ...func(string) error) error {
	var errs []error
	errs = append(errs, GCStaleTemp(ctx, tempDir, dirOnly)...)
	for _, hex := range ids {
		for _, rm := range removers {
			if err := rm(hex); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
