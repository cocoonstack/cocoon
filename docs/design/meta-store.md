# Meta store: unified metadata layer (design v2.8)

Status: design under review (issue #146).
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
short transactions; multi-process CLI safety; multiple engines behind one API
— json and sqlite today, networked engines (redis/etcd/consul-shaped) later
(§8); engine conversion as an explicit ops action (§6).

Non-goals: per-VM runDir sidecars (`config.json` is read by the CH process;
`cocoon.json` is dir-lifecycle-bound), blob/memory files, and operational
flocks (`ops.lock`, clone-locks) — those guard long operations and
directories, not data, and stay on `lock/flock`. No network-filesystem
support for the sqlite engine (§4). **No cross-namespace atomic writes** (§1,
rule 2) — a capability sqlite could offer but json cannot; the API refuses to
expose what an engine cannot honor.

Design calibration (maintainer): no overdesign, no defenses for contrived
scenarios. Every guard in this document must map to an operational risk that
can occur under the deployment contract (single host, ops-driven engine
conversion, no mixed-version fleets). Review findings whose preconditions
require operator action outside that contract are labeled [contrived] and
accepted without code.

## 1. API (package `meta`)

A **namespace** is the unit of ownership and of write atomicity: one
subsystem's record set plus its satellite sets (VM records + that engine's
orphan dirs + its tombstones; images + their refs). It maps to one file in
the json engine and one table group in sqlite — which is exactly today's file
layout.

```go
type CommitMode uint8

const (
    CommitDurable CommitMode = iota // default: fsync-on-commit; matches today's Update
    CommitRelaxed                   // may lose the un-checkpointed tail on power loss; store stays consistent
)

// Scope declares, before the closure runs, every namespace a transaction
// touches. Write is the single namespace it may modify; Read lists the others
// it may read. The declaration is what makes lock ordering possible.
type Scope struct {
    Write string
    Read  []string
}

type Store interface {
    View(ctx context.Context, nss []string, fn func(Reader) error) error
    Update(ctx context.Context, sc Scope, mode CommitMode, fn func(Writer) error) error
    Events(ctx context.Context) (ch <-chan struct{}, release func(), err error)
    Close() error
}
```

Contract clauses (binding on every engine):

1. **Closures are pure and retryable.** `fn` may run more than once (optimistic
   engines, busy retries). All effects go through the Reader/Writer handle.
   Results must be accumulated in state created *inside* `fn` and published to
   the caller only after the transaction returns nil — appending to a
   caller-owned slice from inside `fn` duplicates output on retry. The
   contract-test suite includes a forced-retry engine wrapper that runs every
   closure twice.
2. **Declared scope, single write namespace.** A transaction declares its
   full namespace set up front (`Scope`); a write transaction may modify
   exactly one of them. Engines acquire every declared namespace — write
   target included — in one fixed global order BEFORE invoking the closure.
   Lock-then-discover would deadlock (A writes vms and reads images while B
   writes images and reads vms), which is why the read set is part of the API
   rather than something the engine learns as the closure runs. Touching an
   undeclared namespace fails with `ErrScope` at the first access, not at
   commit. This keeps the json engine correct and first-class (one file
   rewritten atomically per commit; no multi-file commit protocol) and costs
   sqlite nothing (it ignores the ordering hint). Flows needing atomicity
   across subsystems must be redesigned as single-namespace transactions plus
   idempotent reconciliation — this design promises cross-subsystem atomicity
   to no one.
3. **Isolation.** `Update` is serializable within its namespace; `View` sees a
   consistent snapshot of every namespace it touches.
4. **Detached values, every read API.** `Get`, `List`, `Scan`, `Find`,
   `FindAll` and `Log.Scan` all hand back deeply detached values the caller
   may mutate freely; mutation never reaches the store. Engines must not
   expose pointers into their own in-memory state (the json engine decodes
   per call). Persisting a change requires `Replace`.
5. **Durability is enforced structurally, not by caller discipline.** Every
   `Writer` carries its transaction's `CommitMode`; every write defaults to
   requiring `CommitDurable`; the only path to a relaxed write is a per-op
   opt-in (`meta.RelaxedOK`), so every relaxed site is explicit and greppable.
   A durable-default write under a Relaxed `Writer` fails with
   `ErrDurabilityContract` — a contract violation is a test-time error, not a
   power-loss incident.
6. **ctx wins.** Cancellation/deadline preempts engine waiting within one
   retry interval (§4 bounds it for sqlite); a blocked writer never pins a
   caller past its deadline.
7. **Engine-neutral errors.** `ErrNotFound`, `ErrConflict`, `ErrBusy`,
   `ErrCorrupt`, `ErrNoSpace`, `ErrDurabilityContract`; engine codes never
   reach callers.

Collections and logs are storage primitives; lifecycle semantics live in
domain repositories (§1a):

```go
func NewCollection[R any](s Store, ns, table string, opts ...Option[R]) *Collection[R]

func (c *Collection[R]) Get(ctx context.Context, r Reader, id string) (*R, error)          // detached copy
func (c *Collection[R]) Insert(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error  // ErrConflict on id/unique collision
func (c *Collection[R]) Replace(ctx context.Context, w Writer, id string, rec *R, opts ...WriteOpt) error // ErrNotFound if absent
func (c *Collection[R]) Delete(ctx context.Context, w Writer, id string, opts ...WriteOpt) error
func (c *Collection[R]) Scan(ctx context.Context, r Reader, fn func(id string, rec *R) error) error
func (c *Collection[R]) List(ctx context.Context, r Reader) (map[string]*R, error)         // detached; small namespaces
func (c *Collection[R]) Find(ctx context.Context, r Reader, index, value string) (string, *R, error)
func (c *Collection[R]) FindAll(ctx context.Context, r Reader, index, value string, fn func(id string, rec *R) error) error // non-unique indexes

type Seq uint64

func NewLog[E any](s Store, ns, table string) *Log[E]
func (l *Log[E]) Append(ctx context.Context, w Writer, e E, opts ...WriteOpt) (Seq, error)
func (l *Log[E]) Scan(ctx context.Context, r Reader, after Seq, fn func(Seq, E) error) error
```

Log contract: over COMMITTED entries, `Seq` is unique and strictly
increasing within a namespace; gaps are permitted. A number handed to a
transaction that later rolls back MAY be reused by a subsequent append —
required, because SQLite reuses rowids from rolled-back inserts and an
independent allocator cannot write while the enclosing `BEGIN IMMEDIATE`
holds the writer. `Scan(after)` is EXCLUSIVE of `after` and yields in
increasing `Seq` order, so `after = lastSeen` resumes without duplication or
loss. `Append` is bound by clause 5 exactly like collection writes.

`Scan` is callback-shaped so iteration errors propagate and no lazy iterator
escapes the read transaction. Single-op sugar (`c.Get1(ctx, s, id)`) wraps an
auto View/Update.

### 1a. Domain repositories

Reserve/adopt/finalize, quarantine, state transitions and other
compare-and-swap flows are methods on `hypervisor.VMRepository` (and peers),
composed of primitives inside one `Update` with in-transaction revalidation.
Business code never hand-rolls Get+Replace. Follow-up (separate change): with
reserve+name-claim+finalize as one transaction, the `hypervisor.Reserver`
choreography the CLI drives today can shrink or disappear.

## 2. Schema (sqlite engine; one DB, namespace = table group)

JSON payload column everywhere (existing struct tags; low schema churn);
secondary keys and per-row metadata extracted into real columns. **Lossless
representation of today's records is a hard requirement** — the fixtures gate
in §9 enforces it.

```sql
-- Set ONLY when creating a fresh DB; on every open they are READ and verified,
-- never rewritten (§6: wrong application_id or a newer user_version fails closed).
PRAGMA application_id = 0x434F434E;  -- "COCN"
PRAGMA user_version   = 1;           -- schema version; upgrades run as explicit
                                     -- transactional migrations, never implicit

CREATE TABLE meta_state (namespace TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL,
                         source TEXT, sha256 TEXT, records INTEGER, applied_at TEXT);
-- SQLITE-ENGINE CONCEPT ONLY. A DB file cannot distinguish "namespace never
-- initialized" from "namespace empty", so this row does it. The json engine
-- has no equivalent and needs none: the presence or absence of its namespace
-- file is the state, exactly as today — existing json deployments upgrade
-- into the meta refactor untouched and never run a conversion (§6, §8).
-- state: 'initialized' (fresh, possibly zero records) | 'converted' (imported
-- from json). A namespace with NO row is UNINITIALIZED — never "empty" (§6).
-- NOT NULL is explicit on every PK column: SQLite implies it only for a lone
-- INTEGER PRIMARY KEY (rowid alias); every other PK shape admits NULLs.

-- namespace vms_firecracker (same shape for vms_cloudhypervisor)
CREATE TABLE vms_firecracker            (id TEXT NOT NULL PRIMARY KEY, name TEXT UNIQUE, data TEXT NOT NULL);
CREATE TABLE vms_firecracker_orphandirs (path TEXT NOT NULL PRIMARY KEY);
CREATE TABLE vms_firecracker_tombstones (id TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL);

-- namespace snapshots
CREATE TABLE snapshots            (id TEXT NOT NULL PRIMARY KEY, name TEXT UNIQUE, data TEXT NOT NULL);
CREATE TABLE snapshots_tombstones (id TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL);

-- namespace networks
CREATE TABLE networks (id TEXT NOT NULL PRIMARY KEY, vm_id TEXT, data TEXT NOT NULL);
CREATE INDEX networks_by_vm ON networks(vm_id);

-- namespace images_oci (same shape for images_cloudimg)
CREATE TABLE images_oci            (digest TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE image_oci_refs        (ref TEXT NOT NULL PRIMARY KEY, digest TEXT NOT NULL REFERENCES images_oci(digest), data TEXT NOT NULL);
CREATE INDEX image_oci_refs_digest ON image_oci_refs(digest);
CREATE TABLE images_oci_tombstones (digest TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL);
```

Representation rules (each has a round-trip fixture in §9):

- **Per-ref payload survives.** Several refs may share one digest and each
  carries its own fields (ref string, CreatedAt, …) — hence `data` on the refs
  table, not a bare (ref → digest) mapping. Digest-prefix lookup and "all refs
  of a digest" are indexed queries.
- **Sparse names.** An unnamed record (snapshots may legitimately have an
  empty name) stores SQL `NULL`, never `''`: SQLite's UNIQUE treats NULLs as
  distinct but `''` as a value, so empty strings would collide on the second
  unnamed record. Mapping is `name == "" ⇔ NULL` in both directions.
- **Satellite sets stay in their namespace** (orphan dirs, tombstones), so
  every write a subsystem performs is single-namespace (§1 rule 2).

## 3. Durability

- **Default is `CommitDurable`** (sqlite: dedicated `synchronous=FULL`
  connection; json: today's fsync behavior), preserving current `Update`
  semantics for VM finalize/rollback/state/name changes and all
  snapshot/image mutations.
- **`CommitRelaxed`** (sqlite: `synchronous=NORMAL` connection) is used in
  phase 1 by exactly one flow: the creating-placeholder insert
  (`PrereserveVM`/`ReserveVM`) — the flow whose loss GC provably re-derives,
  today's only `UpdateNoDirSync` user. Recorded correction:
  `UpdateNoDirSync` still fsyncs main+`.prev` and skips only the parent-dir
  sync, so this is a deliberate, GC-covered relaxation, not an equivalence.
  VM records are not pure runtime cache (config, blob pins, quarantine) —
  hence durable-by-default.
- Widening the Relaxed set (start/stop flips, GC deletes, network records) is
  gated on later per-operation measurement.
- Engine note (sqlite only, NOT part of the §1 contract): a FULL commit also
  flushes earlier NORMAL commits because they share one WAL. No caller may
  rely on this — see §8's independence rule.

## 4. sqlite runtime contract (modernc.org/sqlite — pure Go; CGO would break cross-compilation)

- DSN-level per-connection config (survives pool growth):
  `_pragma=journal_mode(WAL)`, `busy_timeout(50)`, `foreign_keys(1)`,
  `trusted_schema(0)`, `synchronous(FULL|NORMAL)`; `_txlock=immediate` on
  writer handles.
- **Short busy_timeout + ctx-aware retry loop.** A long in-driver
  `busy_timeout` would sleep past a caller's deadline (clause 6 is otherwise
  unimplementable): the engine retries `BEGIN IMMEDIATE` in a loop bounded by
  ctx, with jittered backoff, surfacing `ErrBusy` only when ctx still has
  budget and the contention persists past the retry ceiling. Contract test:
  a held writer + a 100ms ctx deadline must return a ctx error within ~150ms.
- Handles: `writerDurable`, `writerRelaxed`, each `*sql.DB` with
  `MaxOpenConns=1`; a bounded reader pool (≤ NumCPU); plus one **pinned
  notifier connection** used by nothing else (§7). WAL admits ONE writer at a
  time; `ErrBusy` is a real, mapped outcome.
- Error mapping: SQLITE_BUSY/LOCKED→`ErrBusy`; constraint violations→
  `ErrConflict`; SQLITE_CORRUPT/NOTADB→`ErrCorrupt`; SQLITE_FULL/ENOSPC→
  `ErrNoSpace`; row-missing→`ErrNotFound`.
- Filesystem: WAL requires shared memory; network filesystems are unsupported.
  `doctor` checks the meta dir's fs type and refuses NFS/CIFS/unknown-FUSE.
- Observability: writer-wait, transaction duration, commit duration, WAL
  bytes, checkpoint duration; slow-transaction warn threshold.
- Backup: `VACUUM INTO` a temp path → `PRAGMA integrity_check` on the copy →
  fsync → atomic rename → parent-dir sync. `doctor` runs `integrity_check`.

## 5. GC: revalidating tombstone protocol (replaces lock-all — phase-gated)

No hybrid phase exists: the conversion moves EVERY namespace at once (§6,
§10), so GC never reasons across a split authority. This is a correctness
requirement, not a convenience — the cross-reference graph is effectively
connected (VMs pin image blobs, own networks, and carry snapshot IDs), so a
partial migration would let a GC snapshot taken from the legacy side race a
committed write on the meta side (image GC deleting a blob a just-reserved VM
pinned). The alternative — a compatibility gate every meta write must take —
would serialize exactly what this design exists to parallelize; it is
recorded as the fallback if a partial migration is ever forced, with that
cost stated up front.

Per candidate:

1. Short `View` builds candidates (never held across file IO).
2. Acquire the entity's operational lock (ops flock — unchanged).
3. Short `Update` on the target namespace (Durable): re-read every relevant
   namespace, verify state/references/UpdatedAt still qualify, insert the
   tombstone with a freshly generated `lease_id` and `phase='leased'`, commit.
4. Short `Update`: flip the tombstone to `phase='deleting'` **before touching
   the filesystem**, guarded by `WHERE id=? AND lease_id=?`.
5. Slow file/directory cleanup outside any transaction.
6. Short `Update`: delete the record rows + the tombstone, again guarded by
   `WHERE id=? AND lease_id=?`. Zero rows affected means the lease was
   reclaimed while this worker was slow — abort without deleting anything.

**The `lease_id` is a fencing token.** Every mutating statement after step 3
carries it, so a worker that stalled past its TTL and resumed after another
worker reclaimed the lease affects zero rows instead of deleting the new
lease's target or a reference re-established in the meantime (the ABA case).
Stale-lease reclamation must hold the same entity ops lock as the original
worker, so reclaim and resume cannot interleave.

**Crash recovery is phase-directed — never blind rollback.** A `leased`
tombstone provably predates any filesystem mutation and may be rolled back
(record stays live). A `deleting` tombstone means cleanup may have started:
recovery MUST roll forward (re-run idempotent cleanup, then finalize) and
must keep the tombstone if cleanup fails, so a record whose backing files are
partially gone is never resurrected as live metadata. Reference-creating
transactions check tombstones in their own `Update` and fail `ErrConflict`
for any phase. Concurrent GC runs (scheduled + manual) may pick the same
candidate: `ErrConflict` on the tombstone insert means another worker owns
the lease — skip the candidate, not an error.

## 6. Engine conversion: explicit, offline, operator-driven

Maintainer decisions: backward compatibility across a conversion is NOT a
requirement (one-time, operator-scheduled downtime action), and engine
initialization is an ops step — never implicit library behavior racing
concurrent CLIs. Deployments staying on the json engine (the default) never
convert: the meta refactor is behavior-preserving for them (§8, §9 fixtures).

- **Initialization is explicit.** `cocoon meta init --backend sqlite`
  creates the DB on a fresh root: it sets `application_id`/`user_version`
  (the only time either is written) and writes a `meta_state` row per
  namespace (`state='initialized'`, `records=0`). Absence of a row means
  UNINITIALIZED, never empty — see the open check below. On an existing
  legacy root, `meta convert` performs init and import in one action.
- **Identity and version are verified on every open, never rewritten.**
  A wrong `application_id` (not this application's file) or a `user_version`
  NEWER than the binary understands fails closed with a clear message; an
  older `user_version` is upgraded only by an explicit, transactional schema
  migration, never implicitly at open time.
- **`cocoon meta convert --to sqlite|json`** performs a cutover into a
  **fresh target**: advisory check that no cocoon activity is present (legacy
  locks free); load each namespace via the source engine
  (json source preserves `.prev` recovery); derive the name index from
  records and VERIFY it against the source's own name map (mismatch → abort
  with a report); write all records plus the `meta_state` row
  (`state='converted'`, source path, sha256, record count) in one Durable
  transaction per namespace; commit; then rename the source aside
  (`*.converted-<ts>`) and sync the parent dir.
- **Resumable, never ambiguous.** A crash between "target committed" and
  "source renamed aside" leaves a populated target; the rerun must finish the
  job, not refuse it. On start the tool compares each namespace's target
  `meta_state` row (source path, sha256, record count) against the pending
  source: an exact match is THIS conversion, verified and resumed (remaining
  namespaces + pending renames); a populated target that does not match any
  pending source is a foreign store and is refused, telling the operator to
  move it aside deliberately.
- **The old authority is retired, not left in place.** Conversion in either
  direction renames its source aside only after a fully committed, fully
  verified write, so a later reverse conversion can never find a stale
  authority and skip importing newer data. Test matrix includes
  sqlite→json→(writes)→sqlite with a diff assertion on the final content.
- **Fail-closed open (sqlite engine only).** With `meta_backend: sqlite`,
  opening a namespace with no `meta_state` row refuses: "run
  `cocoon meta convert`" when a legacy source exists, "run `cocoon meta init`"
  when it does not — an uninitialized namespace is never silently treated as
  empty. The **json engine has no such check and needs none**: a missing
  namespace file means empty, exactly as today, so an existing json
  deployment upgrades into the meta refactor with no marker, no format
  change, and no conversion. Running a pre-cutover binary after a conversion
  is unsupported — the renamed-aside source and release notes are the guard,
  not code.
- Tests: crash-and-rerun idempotence at every step boundary; name-index
  mismatch abort; uninitialized refusal; the round-trip matrix above.

## 7. Watch/events (retires the `Watchable.WatchPath` file-watch leak)

`vm status --watch/--event` currently watches `vms.json`; under WAL the main
DB barely changes between checkpoints. `Store.Events` replaces it: fsnotify
on the meta directory filtered to the engine's files (`meta.db{,-wal,-shm}`
for sqlite, the namespace files for json), debounced, and — for sqlite —
confirmed via `PRAGMA data_version` on a **dedicated `*sql.Conn` held for the
notifier's lifetime**. This is mandatory, not an optimization: `data_version`
is only comparable across successive calls on the SAME connection and never
changes for that connection's own commits, so polling it through a pool
(where connections may be replaced under churn) both misses and invents
events. The notifier connection never writes, so every commit it must report
comes from another connection or another process and is therefore visible to
it.

`data_version` is only consulted after a filesystem signal, so a lost signal
would stall `status --event` indefinitely. Overflow and loss are handled
explicitly: on fsnotify overflow or watch error the notifier immediately
re-checks `data_version`, resubscribes, and signals if the value moved; and a
bounded safety poll (low frequency, single `PRAGMA` on the pinned connection)
runs unconditionally as a floor, so no missed inotify event can wedge a
watcher. Contract tests cover same-process commits, external-process commits,
pool churn, and forced overflow.
`Watchable.WatchPath`, `BackendConfig.IndexFile()/IndexLock()` retire together.

## 8. Engines (json and sqlite today; redis/etcd/consul-shaped later)

One API, several engines, chosen per deployment via config
(`meta_backend: json | sqlite`, default `json`). The engine-agnostic
contract-test suite (isolation, retryable closures, detached values, write
scope, error taxonomy, constraint semantics, events) runs against every
engine.

- **json (first-class, default).** Today's per-namespace files, formats, and
  `.prev` crash story move INSIDE the engine: an `Update` loads the target
  namespace, applies primitives, and rewrites that one file atomically under
  its flock; read-only namespaces are locked in a fixed global order. Cost
  profile is explicitly O(records) per write — correct for small and dev
  deployments, and behavior-preserving for existing users (§9 fixtures pin
  the byte format and `.prev` recovery).
- **sqlite (scale engine).** §2–§4; opt-in via config plus one offline
  conversion. This is the engine the C1M fleet runs.
- **Networked engines later (redis / etcd / consul shaped).** Implementable
  without API change: retryable pure closures → etcd STM / redis
  WATCH-MULTI-or-Lua; `CommitDurable/Relaxed` → quorum-or-fsync policy vs
  default; `Events` → watch APIs / keyspace notifications; tombstone leases →
  native leases; single-namespace write scope → single-key-space transaction.
  Recorded caveat: a network store on the claim path conflicts with the
  single-host no-daemon latency model; the realistic role is fleet-level, but
  the boundary holds.

Two portability rules binding on callers and engines:

- **Relaxed independence.** Data written under `CommitRelaxed` must be
  independently acceptable to lose. Callers must never depend on a later
  unrelated Durable commit making earlier Relaxed data durable — that
  ordering is a shared-WAL accident of the sqlite engine (§3) that a sharded
  or per-key-queue engine will not honor.
- **Constraints are semantics, not schema.** The unique-index and
  referential-integrity outcomes of §2 (`ErrConflict` on name/ref collision,
  digest integrity) are part of the API contract; a non-relational engine
  must reimplement them transactionally, not merely store keys and values.

Outside the boundary for every engine: host-local operational locks
(ops/clone flocks) and directory lifecycles.

## 9. Acceptance gates

Engine-scoped, because the engines have deliberately different cost models.

**Correctness (all engines).**
- Contract-test suite incl. the forced-retry wrapper (clause 1), detached-value
  mutation test (clause 4), write-scope violation rejection (clause 2),
  `ErrDurabilityContract` structural check (clause 5), and the ctx-vs-held-writer
  deadline test (clause 6 / §4).
- **Scope enforcement and deadlock freedom**: accessing an undeclared
  namespace returns `ErrScope`; a stress test runs mutually-inverse
  transactions (write A read B vs write B read A) across processes and must
  never deadlock or time out.
- **Log cursor**: `Seq` unique and increasing over committed entries;
  `Scan(after)` never duplicates or skips across a rolled-back append that
  reused a number.
- **Commit atomicity under crash** (all engines): kill the process at every
  step of a multi-record single-namespace `Update` (json: mid-rewrite, between
  temp write and rename, between rename and `.prev` rotation; sqlite: mid-WAL
  append, pre/post commit-frame); reopening must show the transaction wholly
  applied or wholly absent — never partially. Isolation tests alone do not
  prove this.
- **Tombstone fencing / ABA**: worker A leases and stalls past TTL → worker B
  reclaims under the same ops lock and finalizes → A resumes and its guarded
  statements affect zero rows, deleting nothing that B or a subsequent
  reference re-established.
- **Schema identity**: wrong `application_id` and newer `user_version` both
  fail closed on open; no open path ever rewrites either.
- **Round-trip fixtures per namespace**: legacy json corpus → engine → export,
  asserting field-level equality including multi-ref-per-digest payloads,
  unnamed (sparse-name) records, orphan dirs, and quarantine fields.
- **json format fidelity**: golden byte-level fixtures for every namespace
  file, plus `.prev` recovery tests (corrupt main → previous generation
  served) and crash-boundary tests — a behavior-breaking default engine must
  fail here.
- GC: concurrent-candidate `ErrConflict` skip; crash in `leased` (rolls back)
  and in `deleting` (rolls forward, never resurrects) — the latter asserted
  with files already partially removed.
- Conversion: crash-and-rerun idempotence at each step, including a crash
  between target commit and source rename (the rerun must RESUME, not refuse);
  foreign-target refusal; name-mismatch abort; uninitialized-namespace
  refusal (sqlite); json-engine upgrade with no marker and no conversion;
  sqlite→json→writes→sqlite content diff.
- `status --event` regression: same-process commits, external-process
  commits, connection-pool churn (the notifier must keep observing across it),
  and fsnotify queue overflow (a dropped watch event must degrade to a
  data_version-confirmed poll, not a permanently missed change);
  unsupported-fs refusal.
- Power-loss: beyond `integrity_check`, verify domain invariants — VM/name
  bijection, image/ref integrity, network→VM references, tombstone phase
  consistency, directory ownership.

**Performance, sqlite engine (the reason this design exists).**
- Microbench: single-record Insert/Replace/Delete/Get at N = 1/100/1k/10k —
  slope ≈ O(log N), enforced in CI.
- Hardware storms, ≥5 rounds per point (median, p95, variance reported), with
  a **predeclared numeric bound, judged by the upper bound of a 95% CI** —
  not by failure to reject flatness: fitting wall ≈ a + b·N at B=64, the pass
  rule is UCB95(b) ≤ 2 ms per resident record. A noisy O(N) implementation
  fails because its confidence bound exceeds the threshold, which
  "statistically indistinguishable from flat" would have let through.
- Absolute anchors (cocoonstack/sandbox#30; reference testbed 16-core NVMe,
  FC none-lane 512M golden): 3×B64 no-clean ladder round-1 (N=0) wall ≤ 1.5s
  (legacy 1009ms — no small-N regression), later rounds ≤ 1.3× round-1
  (legacy 3.6×/4.9×); phase decomposition at N=128 — the two index-attributed
  segments combined p50 ≤ 300ms (legacy 2347+867ms), i.e. within ~20× of the
  14ms snapshot-load floor instead of ~230×.
- Component metrics per round: writer wait, transaction/commit duration, WAL
  bytes, checkpoint duration, p99 around checkpoint boundaries.
- Mixed workload: B=64 clone storm with a concurrent durable snapshot/image
  write.
- **Real multi-process correctness** (not just load): 256 separate CLI
  processes, not goroutines, asserting successful-operation counts against
  final record counts, name/ref index consistency, that every failure is a
  mapped `ErrConflict`/`ErrBusy` rather than a lost write, and zero
  corruption on reopen. A process-local lock that silently drops records
  would otherwise pass on latency alone.

**Performance, json engine.** No slope gate — O(records) per write is its
documented contract (§8), so an O(log N) requirement would fail a correct
implementation. Gate is non-regression, measured as a **paired ratio**: at
N = 1/100/1k, legacy and meta-json rounds run interleaved on the same host
(paired samples cancel drift), and the pass rule is UCB95 of the
meta/legacy latency ratio ≤ 1.15. An unqualified ±10% single-run threshold
is tighter than the measured ±20% host noise and would both fail unchanged
code and pass real regressions depending on run order.

## 10. Phasing (coupling-honest)

- **P0 (now, no release):** `meta` package + json AND sqlite engines +
  contract-test suite + fixtures + microbenches; `VMRepository` prototype
  behind a build tag. Legacy path untouched.
- **P1:** ALL namespaces move onto the meta API in one release — VMs,
  snapshots, networks, images(+refs). json engine stays the default (internal
  refactor, zero user-visible change, fixtures prove it); sqlite +
  `meta init`/`meta convert` ship alongside, and scale deployments convert
  everything in one downtime window. GC switches to the tombstone protocol in
  the same step and `gc.Module.Locker` is removed. Splitting this across
  releases is not an option: a partial migration splits the cross-reference
  graph and reintroduces the GC race (§5).
- **P2:** measurement and tuning against the §9 gates on the testbed;
  Relaxed-set widening decided per operation with data.
- **P3:** metering Log (optional); retire the OLD `storage/json` + `Store[T]`
  code paths, `Watchable`, `IndexFile/IndexLock` after ≥1 release of soak —
  the json ENGINE behind `meta.Store` remains a supported first-class backend.
- Revertibility: P0 trivially; P1+ only via `meta convert --to json`. The
  earlier "every step revertible" claim is withdrawn.
