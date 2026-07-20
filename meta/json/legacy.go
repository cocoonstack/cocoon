package json

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

// This file is the P0 legacy adapter (design §10): an explicit allowlist for
// the four caller groups that depend on the old store's lock semantics and
// cannot be expressed as pure retryable closures. Nothing else may touch it;
// it is deleted in P1 with the stable VM lock, the per-digest image lock and
// the tombstone-protocol GC.

// RawView reads ns without locking. Allowlist: LockVMOps run-dir resolution
// and the netresize failed-persist re-read — ops verbs must not stall behind
// an in-flight GC cycle's namespace lock.
func (s *Store) RawView(ctx context.Context, ns string, fn func(meta.Reader) error) error {
	st, ok := s.nss[ns]
	if !ok {
		return fmt.Errorf("namespace %q: %w", ns, meta.ErrScope)
	}
	l, err := loadNamespace(ctx, st.def)
	if err != nil {
		return err
	}
	return fn(&txReader{models: map[string]*loaded{ns: l}})
}

// NamespaceLocker exposes ns's flock instance for gc.Module.Locker; the
// legacy GC takes every module's lock first and then reads/writes raw.
func (s *Store) NamespaceLocker(ns string) (lock.Locker, error) {
	st, ok := s.nss[ns]
	if !ok {
		return nil, fmt.Errorf("namespace %q: %w", ns, meta.ErrScope)
	}
	return st.locker, nil
}

// LockedView reads ns under a lock the caller already holds via NamespaceLocker.
func (s *Store) LockedView(ctx context.Context, ns string, fn func(meta.Reader) error) error {
	return s.RawView(ctx, ns, fn)
}

// LockedUpdate writes ns under a lock the caller already holds via
// NamespaceLocker, with legacy WriteRaw semantics: full-sync atomic write,
// no .prev rotation, no post-release fsyncs.
func (s *Store) LockedUpdate(ctx context.Context, ns string, fn func(meta.Writer) error) error {
	st, ok := s.nss[ns]
	if !ok {
		return fmt.Errorf("namespace %q: %w", ns, meta.ErrScope)
	}
	l, err := loadNamespace(ctx, st.def)
	if err != nil {
		return err
	}
	w := &txWriter{
		txReader: txReader{models: map[string]*loaded{ns: l}},
		write:    ns,
		model:    l.model,
		mode:     meta.CommitDurable,
	}
	if err = fn(w); err != nil {
		return err
	}
	data, err := st.def.Codec.Encode(l.model)
	if err != nil {
		return err
	}
	if err := utils.AtomicWriteFile(st.def.FilePath, data, 0o644); err != nil {
		return code(err, meta.ErrIO)
	}
	return nil
}

// ImpureUpdate is Update for the image publish critical sections — OCI pull,
// importTarLayers, importTarFromReader, cloudimg commit — whose closures move
// blob files inside the transaction. The json engine runs closures exactly
// once, which is what makes the exemption sound.
func (s *Store) ImpureUpdate(ctx context.Context, ns string, fn func(meta.Writer) error) error {
	return s.Update(ctx, meta.Scope{Write: ns}, meta.CommitDurable, fn)
}
