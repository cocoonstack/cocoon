# Meta store: unified metadata layer (design v2)

Status: design under review (issue #146; v2 after external review).
Baselines and measurements: cocoonstack/sandbox#30 (2026-07-20 phase decomposition).

## Motivation (summary)

Clone storms are dominated by the metadata layer, not virtualization: at B=64 /
resident N≈190, the segments containing the two VM-index writes carry p50
2347ms + 867ms of a 4069ms clone while the FC snapshot load is 14ms. Every
`storage.Store[T]` update rewrites the whole index plus a `.prev` copy under an
exclusive flock (~1.8KB/record, 339KB×3 IO at N=192) ⇒ storm cost O(N·B).
Removing a single RMW (#132/#145) measured inside host noise — the O(N)
payload per write is the problem. `Store[T]`'s whole-value semantics pin that
cost model; the consumer call profile is record-shaped.

## Goals / non-goals

Goals: record-granularity metadata ops; per-transaction durability classes;
cross-namespace consistent reads and short transactions; multi-process CLI
safety; multiple engines behind one API — json and sqlite today, networked
engines (redis/etcd/consul-shaped) later (§8); engine conversion as an
explicit ops action (§6).

Non-goals: per-VM runDir sidecars (`config.json` is read by the CH process;
`cocoon.json` is dir-lifecycle-bound), blob/memory files, and operational
flocks (`ops.lock`, clone-locks) — those guard long operations and directories,
not data, and stay on `lock/flock`. No network-filesystem support for the
sqlite engine (§4).

Design calibration (maintainer): no overdesign, no defenses for contrived
scenarios. Every guard in this document must map to an operational risk that
can occur under the deployment contract (single host, ops-driven engine
conversion, no mixed-version fleets). Review findings whose preconditions
require operator action outside that contract are labeled [contrived] and
accepted without code.

## 1. API (package `meta`)

```go
type CommitMode uint8

const (
    CommitDurable CommitMode = iota // default: fsync-on-commit; matches today's Update
    CommitRelaxed                   // may lose the un-checkpointed tail on power loss; DB stays consistent
)

type Store interface {
    View(ctx context.Context, fn func(Reader) error) error
    Update(ctx context.Context, mode CommitMode, fn func(Writer) error) error
    // Events delivers a coalesced change signal (see §7). release stops delivery.
    Events(ctx context.Context) (ch <-chan struct{}, release func(), err error)
    Close() error
}
```

Contract clauses (binding on every engine):

1. **Closures are pure and retryable.** `fn` may be executed more than once by
   an engine that resolves contention optimistically; all effects must go
   through the Reader/Writer handle, none outside it. (This is what keeps a
   future networked engine implementable behind the same API — §8.)
2. **Isolation.** `Update` runs serializable; `View` sees a consistent
   snapshot. `CommitMode` is a per-transaction property chosen before BEGIN.
3. **Durability is enforced structurally, not by caller discipline.** Every
   `Writer` carries its transaction's `CommitMode`; every collection write
   defaults to requiring `CommitDurable`. The only way to write under a
   Relaxed transaction is a per-operation opt-in
   (`c.Insert(ctx, w, id, rec, meta.RelaxedOK)`), making every relaxed write
   site explicit and greppable. A durable-default write invoked on a Relaxed
   `Writer` fails with `ErrDurabilityContract` — a power-loss contract
   violation is a test-time error, not an incident.
4. **ctx wins.** Context cancellation/deadline preempts engine busy-waiting.
5. **Engine-neutral errors.** `ErrNotFound`, `ErrConflict`, `ErrBusy`,
   `ErrCorrupt`, `ErrNoSpace`, `ErrDurabilityContract`; engine codes never
   reach callers.

Collections are storage primitives only; lifecycle semantics live in domain
repositories (§1a):

```go
func NewCollection[R any](s Store, table string, opts ...Option[R]) *Collection[R]

func (c *Collection[R]) Get(ctx context.Context, r Reader, id string) (*R, error)
func (c *Collection[R]) Insert(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error  // ErrConflict on id/unique-index collision
func (c *Collection[R]) Replace(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error // ErrNotFound if absent
func (c *Collection[R]) Delete(ctx context.Context, w Writer, id string, opts ...WriteOpt) error
func (c *Collection[R]) Scan(ctx context.Context, r Reader, fn func(id string, rec *R) error) error
func (c *Collection[R]) List(ctx context.Context, r Reader) (map[string]*R, error) // detached; small namespaces
func (c *Collection[R]) Find(ctx context.Context, r Reader, index, value string) (string, *R, error)

func NewLog[E any](s Store, table string) *Log[E] // Append(ctx, w, e, opts...) / Scan(ctx, r, from, fn)
// Log.Append is bound by the same Durable-default / RelaxedOK enforcement as
// collection writes (contract clause 3); it is not exempt.
```

`Scan` is callback-shaped so iteration errors propagate and the read
transaction's lifetime never escapes to the caller (no lazy iterator pinning a
WAL snapshot). Single-op sugar (`c.Get1(ctx, s, id)` etc.) wraps an auto
View/Update.

### 1a. Domain repositories

Reserve/adopt/finalize, quarantine, state transitions and other
compare-and-swap flows are methods on `hypervisor.VMRepository` (and peers),
composed of primitives inside one `Update` with in-transaction revalidation.
Business code never hand-rolls Get+Put; that is how `Collection` avoids
becoming a CRUD-mechanism abstraction. Follow-up (separate change): with
reserve+name-claim+finalize as transactions, the `hypervisor.Reserver`
choreography the CLI drives today can shrink or disappear.

## 2. Schema (sqlite engine; one DB, per-namespace tables)

JSON payload column everywhere (existing struct tags; low schema churn);
secondary keys extracted into real columns. Uniform template where it fits,
explicit exceptions where it does not:

```sql
PRAGMA application_id = 0x434F434E;  -- "COCN"
PRAGMA user_version   = 1;

CREATE TABLE migrations (version INTEGER NOT NULL, namespace TEXT NOT NULL,
                         source TEXT, sha256 TEXT, records INTEGER,
                         applied_at TEXT, PRIMARY KEY (version, namespace));
-- NOT NULL is explicit on every PK column: SQLite implies it only for a
-- lone INTEGER PRIMARY KEY (rowid alias); every other PK shape admits NULLs.

CREATE TABLE vms_firecracker      (id TEXT NOT NULL PRIMARY KEY, name TEXT UNIQUE, data TEXT NOT NULL);
CREATE TABLE vms_cloudhypervisor  (id TEXT NOT NULL PRIMARY KEY, name TEXT UNIQUE, data TEXT NOT NULL);
CREATE TABLE vm_orphan_dirs       (engine TEXT NOT NULL, path TEXT NOT NULL, PRIMARY KEY (engine, path));
CREATE TABLE snapshots            (id TEXT NOT NULL PRIMARY KEY, name TEXT UNIQUE, data TEXT NOT NULL);
CREATE TABLE networks             (id TEXT NOT NULL PRIMARY KEY, vm_id TEXT, data TEXT NOT NULL);
CREATE INDEX networks_by_vm ON networks(vm_id);
CREATE TABLE images_oci           (digest TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE image_oci_refs       (ref TEXT NOT NULL PRIMARY KEY, digest TEXT NOT NULL REFERENCES images_oci(digest));
CREATE TABLE images_cloudimg      (digest TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE image_cloudimg_refs  (ref TEXT NOT NULL PRIMARY KEY, digest TEXT NOT NULL REFERENCES images_cloudimg(digest));
CREATE TABLE tombstones           (ns TEXT NOT NULL, id TEXT NOT NULL, leased_at TEXT, PRIMARY KEY (ns, id));
```

Notes: image refs are many-to-one on digest (ref list and digest-prefix
lookups are real queries, not a unique `Find`); `vm_orphan_dirs` is its own
set, not a field smuggled into a record row; `tombstones` carries the GC
deletion leases (§5).

## 3. Durability

- **Default is `CommitDurable`** (sqlite: dedicated `synchronous=FULL`
  connection; fsync per commit). This preserves today's `Update` semantics for
  VM finalize/rollback/state/name changes and all snapshot/image mutations.
- **`CommitRelaxed`** (sqlite: `synchronous=NORMAL` connection) is used in
  phase 1 by exactly one flow: the creating-placeholder insert
  (`PrereserveVM`/`ReserveVM`) — the flow whose loss GC provably re-derives,
  today's only `UpdateNoDirSync` user. Correction recorded from review:
  `UpdateNoDirSync` still fsyncs main+`.prev` and skips only the parent-dir
  sync, so mapping placeholders to Relaxed is a deliberate, GC-covered
  relaxation, not an equivalence. VM records are not pure runtime cache
  (config, blob pins, quarantine) — hence durable-by-default.
- Widening the Relaxed set (start/stop flips, GC deletes, network records) is
  gated on row-granularity hardware measurements, per operation, later.
- Engine note (sqlite only, NOT part of the §1 contract): a FULL commit also
  flushes earlier NORMAL commits because they share one WAL. No caller may
  rely on this — see §8's independence rule.

## 4. sqlite runtime contract (modernc.org/sqlite — pure Go; CGO would break cross-compilation)

- DSN-level per-connection config (survives pool growth):
  `_pragma=journal_mode(WAL)`, `busy_timeout(5000)`, `foreign_keys(1)`,
  `trusted_schema(0)`, `synchronous(FULL|NORMAL)`; `_txlock=immediate` on
  writer handles.
- Handles: `writerDurable` and `writerRelaxed` `*sql.DB` each with
  `MaxOpenConns=1`; reader pool bounded (≤ NumCPU). WAL admits ONE writer at a
  time — `BEGIN IMMEDIATE` can still return busy under contention;
  `busy_timeout` is a wait policy, not a guarantee; `ErrBusy` is a real,
  mapped outcome and ctx deadline preempts the wait.
- Error mapping: SQLITE_BUSY/LOCKED→`ErrBusy`; constraint violations→
  `ErrConflict`; SQLITE_CORRUPT/NOTADB→`ErrCorrupt`; SQLITE_FULL/ENOSPC→
  `ErrNoSpace`; row-missing→`ErrNotFound`.
- Filesystem: WAL requires shared memory; network filesystems are unsupported.
  `doctor` checks the meta dir's fs type and refuses NFS/CIFS/unknown-FUSE.
- Observability: writer-wait duration, transaction duration, commit duration,
  WAL bytes, checkpoint duration; slow-transaction warn threshold.
- Backup: `VACUUM INTO` a temp path → `PRAGMA integrity_check` on the copy →
  fsync → atomic rename → parent-dir sync. `doctor` runs `integrity_check`.

## 5. GC: revalidating tombstone protocol (replaces lock-all — phase-gated)

Hybrid phase rule: while any cross-referencing namespace still lives in JSON,
`gc.Module.Locker` and the lock-all orchestration REMAIN as today (DB-backed
modules join via a locker shim honoring the same order). The protocol below
activates only when all namespaces participating in cross-references are in
the DB (start of P3). "Independently shippable" is scoped accordingly.

Post-migration cycle, per candidate:

1. Short `View` builds candidates (never held across file IO).
2. Acquire the entity's operational lock (ops flock — unchanged).
3. Short `Update` (Durable): re-read all relevant namespaces; verify
   state/references/UpdatedAt still qualify; insert `tombstones(ns,id)`
   (deletion lease); commit.
4. Slow file/directory cleanup outside any transaction.
5. Short `Update`: verify the tombstone is still owned → delete rows + tombstone.

Reference-creating transactions (clone pinning an image, snapshot lease) check
`tombstones` inside their own `Update` and fail `ErrConflict` if the target is
being deleted. Startup sweeps stale tombstones by lease age (crash recovery:
either finish the delete or roll the lease back). Concurrent GC runs
(scheduled + manual) may select the same candidate: `ErrConflict` on the
tombstone insert means another worker owns the lease — the candidate is
skipped, not treated as a GC error.

## 6. Engine conversion: explicit, offline, operator-driven

Maintainer decisions: backward compatibility across a conversion is NOT a
requirement (one-time, operator-scheduled downtime action), and engine
initialization is an ops step — never an implicit library behavior racing
concurrent CLIs. Deployments that stay on the json engine (the default)
never run a conversion at all: the meta refactor is behavior-preserving for
them (§8).

- `cocoon meta convert --to sqlite` (invocable standalone or via doctor)
  performs the cutover explicitly: advisory check that no cocoon activity is present
  (legacy flocks free), take the legacy flocks, load each namespace via the
  legacy loader (preserving `.prev` recovery), derive the name index from
  records and VERIFY it against the legacy Names map (mismatch → abort with a
  report), insert everything in one Durable transaction, write the migrations
  row {namespace, source, sha256, record count}, commit, then rename the JSON
  files to `.imported` and sync the parent dir.
- Crash handling is idempotent re-run under the downtime contract: the tool
  re-checks migrations rows and completes pending renames. There is no
  concurrent-open state machine and no marker choreography — those protected
  mixed-version fleets, which are out of scope by decision.
- With `meta_backend: sqlite` configured, open fails closed when a legacy
  JSON index exists for a namespace with no migrations row → refuse with
  "run cocoon meta convert" (read-only check). Running a pre-cutover binary after migration is
  unsupported — the renamed `.imported` files and release notes are the
  guard, not code.
- Rollback is the same tool in reverse (`cocoon meta convert --to json`) —
  never an old binary.
- Tests: crash-and-rerun idempotence at each step boundary; name-index
  mismatch abort; unmigrated-JSON refusal.

## 7. Watch/events (retires the `Watchable.WatchPath` file-watch leak)

`vm status --watch/--event` currently watches `vms.json`; under WAL the main
DB barely changes between checkpoints. Replace with `Store.Events`: fsnotify
on the meta directory filtered to `meta.db{,-wal,-shm}`, debounced, confirmed
via `PRAGMA data_version` before signaling; one shared notifier per Store.
`Watchable.WatchPath`, `BackendConfig.IndexFile()/IndexLock()` retire together.

## 8. Engines (json and sqlite today; redis/etcd/consul-shaped later)

One API, several engines, chosen per deployment via config
(`meta_backend: json | sqlite`, default `json`). The engine-agnostic
contract-test suite (isolation, retryable closures, error taxonomy,
constraint semantics, events) runs against every engine.

- **json (first-class, default).** Today's per-namespace files, formats, and
  `.prev` crash story move INSIDE the engine: every `Update` loads and
  rewrites the namespace file under its flock. Documented cost profile is
  O(records) per write — right for small and dev deployments, and the meta
  refactor is behavior-preserving for them: json-engine users never convert.
- **sqlite (scale engine).** §2–§4; opt-in via config plus one offline
  conversion (§6). This is the engine the C1M fleet runs.
- **Networked engines later (redis / etcd / consul shaped).** The contract is
  deliberately implementable by them without API change: retryable pure
  closures → etcd STM / redis WATCH-MULTI-or-Lua; `CommitDurable/Relaxed` →
  quorum-or-fsync policy vs default; `Events` → watch APIs / keyspace
  notifications; tombstone leases (§5) → native leases; errors and ctx are
  engine-neutral — nothing in the API names files, locks, fsync, or WAL.
  Recorded caveat: a network store on the claim path conflicts with the
  single-host no-daemon latency model; the realistic role is fleet-level,
  but the boundary holds.

Two portability rules binding on callers and engines:

- **Relaxed independence.** Data written under `CommitRelaxed` must be
  independently acceptable to lose. Callers must never depend on a later
  unrelated Durable commit making earlier Relaxed data durable — that
  ordering is a shared-WAL accident of the sqlite engine (§3 note) that a
  sharded or per-key-queue engine will not honor.
- **Constraints are semantics, not schema.** The unique-index and
  referential-integrity outcomes of §2 (`ErrConflict` on name/ref collision,
  digest FK integrity) are part of the API contract; a non-relational engine
  must reimplement them transactionally, not merely store keys and values.
  The contract-test suite encodes them engine-agnostically.

What deliberately stays OUTSIDE the boundary for every engine: host-local
operational locks (ops/clone flocks) and directory lifecycles — a networked
engine does not change them.

## 9. Acceptance gates (statistical, not single-run)

- Microbench (unit-level, per engine): single-record Insert/Replace/Delete/Get
  at N = 1 / 100 / 1k / 10k — slope ≈ O(log N); enforced in CI.
- Hardware storms: ≥5 rounds per point; report median, p95, variance.
  Regression of wall vs resident-N with a bounded N-coefficient (target:
  statistically indistinguishable from flat), replacing the earlier
  "±10% between two runs" gate that contradicted measured ±20% host noise.
- Per-round component metrics: writer wait, transaction duration, commit
  duration, WAL bytes, checkpoint duration; p99 around checkpoint boundaries.
- Mixed workload: B=64 clone storm with a concurrent durable snapshot/image
  write.
- Real multi-process: 256 separate CLI processes (not goroutines).
- Functional: `status --event` regression; convert crash-and-rerun
  idempotence; unconverted-JSON refusal; unsupported-filesystem refusal;
  ErrDurabilityContract structural check (durable-default write under a
  Relaxed Writer fails; RelaxedOK sites succeed); concurrent-GC
  same-candidate race (second worker's tombstone insert gets ErrConflict and
  skips, GC cycle completes cleanly).
- Power-loss: beyond `integrity_check`, verify domain invariants — VM/name
  bijection, image/ref integrity, network→VM references, tombstone
  consistency, directory ownership.
- **Absolute targets** (shape gates alone would admit a flat-but-slow
  implementation; anchors from cocoonstack/sandbox#30, reference testbed
  16-core NVMe, FC none-lane 512M golden):
  - 3×B64 no-clean ladder: round-1 (N=0) wall ≤ 1.5s (json baseline 1009ms —
    no regression at small N), every later round ≤ 1.3× round-1 (json
    baseline: 3.6×/4.9×);
  - phase decomposition at N=128 (t13 anchor method): the two index-attributed
    segments combined p50 ≤ 300ms (json baseline: 2347ms + 867ms), i.e. the
    metadata layer moves to within ~20× of the 14ms snapshot-load floor
    instead of ~230×.

## 10. Phasing (coupling-honest)

- **P0 (now, no release):** `meta` package + json AND sqlite engines +
  contract-test suite + microbenches; `VMRepository` prototype behind a
  build tag. Legacy path untouched.
- **P1:** VM index moves onto the meta API — json engine as default (pure
  internal refactor, zero user-visible change), sqlite + `meta convert`
  shipping in the same release; scale deployments opt in during a downtime
  window; GC in hybrid lock-all mode.
- **P2:** snapshots → networks → images(+refs) move onto the meta API with
  conversion support, same pattern.
- **P3:** with all cross-referencing namespaces in the DB, GC switches to
  the tombstone protocol and `gc.Module.Locker` is removed.
- **P4:** metering Log (optional); retire the OLD `storage/json` + 
  `Store[T]` code paths, `Watchable`, `IndexFile/IndexLock` after ≥1 release
  of soak — the json ENGINE behind `meta.Store` remains a supported
  first-class backend.
- Revertibility, stated precisely: P0 trivially; P1+ only via the export
  tool. The earlier "every step revertible" claim is withdrawn.
