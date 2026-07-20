# Meta store: unified metadata layer (design v2.14)

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
   `ErrCorrupt`, `ErrNoSpace`, `ErrIO`, `ErrDurabilityContract`, `ErrScope`;
   engine codes never reach callers, and every one is `errors.Is`-comparable.

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
each composed of primitives inside ONE short `Update` with in-transaction
revalidation. Business code never hand-rolls Get+Replace.

**Every destructive flow uses the §5 phase protocol, not just GC.**
`vm rm`, `snapshot rm` and NIC teardown perform the same slow destructive
work outside a transaction; without a phase transition a crash mid-teardown
leaves live metadata over partially removed resources — the exact failure the
protocol exists to prevent. GC is one caller of the protocol, not its owner.

Create/clone stays **two short transactions around slow external work**, not
one: `reserve + name claim` → image prep, directories, CNI, VMM launch (none
of which may sit inside a transaction) → `finalize`. The win is not fewer
transactions but their cost: each becomes a row-level write instead of a
whole-index rewrite, and the placeholder that makes GC's ownership window
work is unchanged. Follow-up (separate change): with reserve and name claim
atomic in one row-level transaction, the `hypervisor.Reserver` choreography
the CLI drives today can shrink.

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
CREATE TABLE vms_firecracker_tombstones (id TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL, payload TEXT NOT NULL);

-- namespace snapshots
CREATE TABLE snapshots            (id TEXT NOT NULL PRIMARY KEY, name TEXT UNIQUE, data TEXT NOT NULL);
CREATE TABLE snapshots_tombstones (id TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL, payload TEXT NOT NULL);

-- namespace networks
CREATE TABLE networks            (id TEXT NOT NULL PRIMARY KEY, vm_id TEXT, data TEXT NOT NULL);
CREATE INDEX networks_by_vm ON networks(vm_id);
CREATE TABLE networks_tombstones (vm_id TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL, payload TEXT NOT NULL);
-- Keyed by VM, not by NIC: network records are one row per NIC, but GC
-- candidates, netns, TAPs and the ops lock are all per VM. A VM-keyed lease
-- also covers the case where no network row survives but an orphan netns
-- does. CNI Add checks this table by vm_id before creating anything.
-- Finalize acts on exactly what the payload says: an AGGREGATE teardown
-- removes every row for that vm_id plus the netns and TAPs derived from it,
-- while a SUBSET teardown (a `vm net remove --index N` resize) removes only
-- the listed NIC indices and leaves the netns and the other NICs alone.
-- Without that distinction, recovering a one-NIC resize would tear down the
-- VM's whole networking.

-- namespace images_oci (same shape for images_cloudimg)
CREATE TABLE images_oci            (digest TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE image_oci_refs        (ref TEXT NOT NULL PRIMARY KEY, digest TEXT NOT NULL REFERENCES images_oci(digest), data TEXT NOT NULL);
CREATE INDEX image_oci_refs_digest ON image_oci_refs(digest);
CREATE TABLE images_oci_tombstones (digest TEXT NOT NULL PRIMARY KEY, lease_id TEXT NOT NULL, phase TEXT NOT NULL, leased_at TEXT NOT NULL, payload TEXT NOT NULL);
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
- **Every GC'd namespace has tombstones, networks included.** Network
  teardown does slow work outside any transaction (CNI DEL, TAP, netns), so
  it needs the same phase marker as the others: without it, a crash mid-
  teardown leaves a live network record pointing at half-released host
  resources, and nothing records that teardown had begun. (The ID-reuse
  variant of this race is not reachable — VM IDs are random 26-char and TAP
  and netns names derive from them — but the crash variant is, and uniformity
  costs nothing.)

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
  `ErrNoSpace`; SQLITE_IOERR/CANTOPEN/READONLY and their json-engine
  equivalents→`ErrIO`; row-missing→`ErrNotFound`.
- Filesystem: WAL requires shared memory; network filesystems are
  unsupported. The check is ENFORCED in open, init and convert — refusing
  NFS/CIFS/unknown-FUSE before any WAL work — because an operator who never
  ran `doctor` would otherwise run SQLite outside its supported locking
  contract. `doctor` reports the same check as diagnostics.
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

**Recovery precedes discovery.** Every cycle first scans existing tombstones
— under each entity's lock, taking over by replacing `lease_id` — and
resumes them by phase. A `deleting` tombstone whose data is already gone will
never reappear as a candidate through record-based discovery, and the
insert-conflict rule would make a later worker skip it, stranding the
tombstone forever. Only after that sweep does the cycle look for new
candidates.

Per candidate:

1. Short `View` builds candidates (never held across file IO).
2. Acquire the entity's operational lock (ops flock) — but only at the
   OUTERMOST lifecycle operation. flock is not reentrant, and callers like
   CH's network resize already hold the VM ops lock before invoking NIC
   removal (`hypervisor/cloudhypervisor/netresize.go` takes `LockVMOps`
   first), so an unconditional re-acquire inside the protocol would
   self-deadlock. Destructive repository flows therefore expose `...Locked`
   entrypoints that assert the lock is already held and skip step 2; the
   plain entrypoints acquire it. The mapping is
   explicit, because it is not uniform today: VMs use the existing per-VM
   `ops.lock`; networks are covered by their owning VM's lock — which is a
   CHANGE, not a description: `cmd/vm/lifecycle.go` tears down TAPs and calls
   `netProvider.Delete` after `hyper.Delete` has already removed the record,
   outside any ops lock, and today it could not do otherwise because the lock
   inode dies with the runDir. Relocating the ops lock (the rule above) is
   precisely what makes network teardown under the owning VM's lock
   implementable, and P1 must restructure `vm rm` to hold it across both the
   record deletion and the network teardown; snapshots use their existing read-lease mechanism in
   exclusive mode — its `LeasePath(id)` is already a SIBLING of the data dir
   (`<dir>.lease`), deliberately outside the tree that gets removed;
   **images have no per-digest lock today and gain one** — a lock file beside
   the blob, taken by GC and by any flow that materializes or re-pins that
   digest. Without it, step 2 and the recovery rule below are unimplementable
   for image GC once `gc.Module.Locker` is gone. Two rules come with it, both
   already established practice in this codebase rather than new invention:
   the lock file is NEVER removed by the cleanup that deletes what it guards
   (`images/gc.go`'s stale-temp sweep already skips `.lock` files for exactly
   this reason — flock synchronizes on the inode, so deleting one races a
   live holder), and a flow pinning several digests takes their locks in
   sorted digest order so two concurrent multi-digest pins cannot deadlock.
3. Short `Update` on the target namespace (Durable): re-read every relevant
   namespace, verify state/references/UpdatedAt still qualify, insert the
   tombstone with a freshly generated `lease_id`, `phase='leased'` AND its
   complete `payload`, commit. The payload is known here — it is derived from
   the candidate — and the column is `NOT NULL` precisely so a tombstone can
   never exist without the information its recovery needs. Step 4 only flips
   the phase; it adds nothing.
4. Short `Update`, committed **before any filesystem work** and guarded by
   `WHERE ... AND lease_id=?`: flip the tombstone to `phase='deleting'`,
   copying the paths cleanup needs into the tombstone so a recovering worker
   needs nothing from the record.
5. Slow file/directory cleanup outside any transaction, driven by the
   tombstone's payload so a recovering worker needs nothing from the deleted
   record.
6. Short `Update`: delete any remaining record rows and the tombstone,
   guarded by `WHERE ... AND lease_id=?`. Zero rows affected means this
   worker is a resumed or duplicated instance of work whose lease was already
   recovered and finalized by another worker after the original owner's
   process died — abort without deleting anything.

**Lock inodes are never destroyed while held — a precondition of this whole
protocol.** flock synchronizes on the inode, so deleting a lock file a worker
holds lets another process create a fresh file at the same path and believe
it owns the lock; both then run the destructive path concurrently. Today two
paths violate this: `ops.lock` sits INSIDE the VM runDir that
`RemoveAll` erases, and snapshot cleanup explicitly
`os.Remove(LeasePath(id))` after removing the data dir. The rule this design
adopts, uniform across namespaces:

- every entity lock file lives OUTSIDE the resource's cleanup set (VM
  `ops.lock` relocates to a runDir sibling, matching where snapshots already
  put `LeasePath`);
- destructive cleanup NEVER deletes a lock file;
- lock files are reaped only by an explicit maintenance action (doctor /
  `gc --deep`) that requires a quiescent store, never during normal
  operation — entity locks are cheap to keep and expensive to get wrong;
- and because unlink-versus-acquire can still split exclusion (a waiter
  blocked on the old inode is granted it after the reaper unlinks the path,
  while a new actor locks the freshly created file at the same path), EVERY
  locker validates identity after acquiring: stat the path, compare
  device+inode with the fstat of the descriptor it holds, and retry on
  mismatch. This is the same hazard `hypervisor/gc.go`'s
  `sweepStaleCloneLocks` documents ("a live waiter can't be split onto a
  fresh inode") and that `images/gc.go` sidesteps by never deleting `.lock`
  files at all.

With that rule the protocol needs no per-namespace special case: the record
is deleted at finalize (step 6) everywhere.

**Lock lifetime: one owner, start to finish.** The entity ops lock taken in
step 2 is held through step 6. A live-but-slow worker is therefore never
preempted — there is no TTL-based stealing, which would be unsound while the
owner may still be writing. Another worker can only take over after the
owner's process is gone and the OS has released the flock; it then finds a
`leased` or `deleting` tombstone and recovers it (§ below) under the lock it
now holds. No filesystem deletion ever happens without holding the lock AND
re-verifying the lease in the same transaction.

**The `lease_id` is a fencing token across process death, not across
stalls.** Recovery takes a NEW `lease_id`, and every mutating statement
carries the holder's own value, so a resumed or re-run instance of the dead
owner's work (an interrupted tool invoked again, a duplicated recovery)
affects zero rows instead of finalizing someone else's lease or deleting a
target whose reference was re-established meanwhile.

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

- **Initialization is explicit and atomic.** `cocoon meta init --backend
  sqlite` creates the DB on a fresh root: schema DDL, `application_id`,
  `user_version` and one `meta_state` row per namespace
  (`state='initialized'`, `records=0`) all commit in ONE transaction —
  SQLite's DDL is transactional, so a crash mid-init leaves either nothing or
  a fully initialized store. A file that exists but carries no `meta_state`
  row at all is recognized as a failed init and restarted (its content is by
  definition worthless); anything else is left alone for the operator. Absence of a row means
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
  with a report); write all records plus — for a SQLITE target only, since
  json has no such concept — the `meta_state` row
  (`state='converted'`, source path, sha256, record count) in one Durable
  transaction per namespace; commit; then rename the source aside
  (`*.converted-<ts>`) and sync the parent dir.
- **Resumable, never ambiguous — via a standalone manifest.** A crash
  between "target committed" and "source renamed aside" leaves a populated
  target; the rerun must finish the job, not refuse it. Target-side state
  cannot answer "is this mine?" in general, because `meta_state` is a
  sqlite-engine concept and a json target has no equivalent — so the tool
  keeps its own engine-independent manifest at the meta root
  (`meta-convert.manifest`): source identity (paths + per-namespace sha256 +
  record counts), target engine, per-namespace completion marks, and aside
  paths. Because the manifest is the ONLY recovery authority for a json
  target, it carries its own crash protocol:
  - it is written and fsynced (temp → fsync → rename → parent-dir fsync)
    **before the first byte is written to the target**, and every update uses
    the same sequence;
  - a namespace whose target data is committed but whose completion mark is
    missing (the crash window between the two) is re-verified on rerun —
    content hash and record count against the source — and claimed as
    complete rather than redone or refused;
  - verification is target-shaped: a sqlite target can additionally check its
    `meta_state` row, a json target has none and is verified purely by
    content hash and record count;
  - a populated target with no matching manifest entry is foreign and
    refused, telling the operator to move it aside deliberately;
  - the manifest is removed, and its parent dir synced, only after the whole
    conversion is complete.
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

The change token is only consulted after a filesystem signal, so a lost
signal would stall `status --event` indefinitely. Overflow and loss are
handled explicitly and **per engine, since the token differs**: sqlite uses
`data_version` on the pinned connection; the json engine, which has no such
counter, uses each namespace file's identity tuple (inode, size, mtime —
already the thing its writers rotate atomically). On fsnotify overflow or
watch error the notifier immediately re-reads its token, resubscribes, and
signals if it moved; and a bounded low-frequency safety poll of the same
token runs unconditionally as a floor, so no missed inotify event can wedge
a watcher on either engine. Contract tests — same-process commits,
external-process commits, pool churn, forced overflow — run against BOTH
engines.
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
  `Scan(after)` never duplicates or skips across a rolled-back append —
  asserted for BOTH legal engine behaviours, the number reused by a later
  append and the number burned as a gap, since the contract permits either
  and a json engine will typically burn where sqlite reuses.
- **Commit atomicity under crash** (all engines): kill the process at every
  step of a multi-record single-namespace `Update`, in the order the code
  actually performs them — json: `.prev` link+rename rotation FIRST, then the
  main temp write and rename, then the file fsyncs, then the parent-dir
  fsync; sqlite: mid-WAL append, pre/post commit-frame. Reopening must show
  the transaction wholly applied or wholly absent — never partially.
  Isolation tests alone do not prove this.
- **Lock-inode safety**: for every namespace, assert that a full destructive
  cleanup leaves the entity's lock file intact (it is outside the cleanup
  set), that a worker holding it still holds the SAME inode afterwards, and
  that the reaper removes a lock file only when `TryLock` proves it unheld.
- **Subset teardown recovery**: crash mid-`deleting` on a SUBSET lease (a
  one-NIC `vm net remove`), then recover — the untouched NIC rows, their
  TAPs, and the netns must all survive. A generic aggregate-shaped GC test
  passes while this fails, which is exactly why it is called out separately.
- **Tombstone fencing / ABA**: kill worker A mid-`deleting` → B acquires the
  released ops lock, recovers under a NEW `lease_id` and finalizes → replaying
  A's exact finalize statement affects zero rows. Also assert the negative
  case: while A is alive and slow, B cannot acquire the lock or steal the
  lease at all.
- **Schema identity**: wrong `application_id` and newer `user_version` both
  fail closed on open; no open path ever rewrites either.
- **Durability, asserted not assumed**: with engine-level fault injection,
  an acknowledged `CommitDurable` write MUST survive simulated power loss
  (catches a `writerDurable` mis-wired to NORMAL, or a json engine that
  skipped an fsync), and a `CommitRelaxed` write is ALLOWED to disappear —
  the old-or-new atomicity and invariant gates pass either way, so this needs
  its own assertion.
- **Backup fidelity** (sqlite): take a backup while a writer is active and
  the WAL is non-empty, then restore it — every acknowledged commit must be
  present (a backup that silently omits un-checkpointed WAL state passes
  every other gate); and assert that the previously published backup stays
  intact and usable until the replacement is fully committed, with crashes
  injected at each step of the temp → verify → fsync → rename → parent-sync
  sequence.
- **Init atomicity**: crash injected during `meta init` leaves either no
  store or a fully initialized one; a file with schema but zero `meta_state`
  rows is recognized as a failed init and restarted, while any other
  unexpected content is refused.
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
  between target commit and source rename, and a crash between a namespace's
  target commit and its manifest completion mark (the rerun must RESUME by
  re-verifying hash/count, not redo or refuse);
  foreign-target refusal; name-mismatch abort; uninitialized-namespace
  refusal (sqlite); json-engine upgrade with no marker and no conversion;
  sqlite→json→writes→sqlite content diff.
- `status --event` regression, run against BOTH engines: same-process
  commits, external-process commits, connection-pool churn (the notifier must
  keep observing across it), and fsnotify queue overflow (a dropped watch
  event must degrade to a change-token-confirmed poll, not a permanently
  missed change); unsupported-fs refusal.
- Power-loss: beyond `integrity_check`, verify domain invariants — VM/name
  bijection, image/ref integrity, network→VM references, tombstone phase
  consistency, directory ownership. Tombstone legality is conditional on the
  payload's candidate kind, not absolute: for a RECORD-BACKED candidate the
  row stays live alongside a `leased` or `deleting` tombstone until finalize
  (§5 step 6), so row-present is the normal in-flight state; for a RECORDLESS
  orphan (a directory or blob GC collects that never had a row) there is no
  row in any phase, and demanding one would fail a correct implementation.
  The genuinely illegal states are a tombstone with an empty `lease_id` or
  payload, a record-backed tombstone whose row vanished before finalize, and
  a `deleting` tombstone that no recovery sweep can act on because its
  payload does not name its cleanup target.

**Performance, sqlite engine (the reason this design exists).**
- Microbench: single-record Insert/Replace/Delete/Get at N = 1/100/1k/10k,
  with a NUMERIC pass rule, since "≈ O(log N)" is unenforceable and an O(N)
  implementation with a small constant would satisfy the storm bounds below:
  per-operation cost at N=10k divided by cost at N=100 must have a 95% CI
  upper bound ≤ 2.0 (log-growth over two decades is ≈1.5×; linear growth
  would be ≈100×), AND the absolute per-operation median at N=10k must stay
  under a concrete ceiling — on the reference testbed (16-core, NVMe):
  Durable single-record write ≤ 5ms (one fsync plus overhead), Relaxed write
  ≤ 1ms, `Get` ≤ 0.5ms. Without concrete numbers a flat-but-slow primitive
  (say 500ms per op) satisfies the ratio rule trivially.
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

## 10. Phasing (one engine at a time; the boundary is proven before it is exploited)

Sequencing decision (maintainer): **do not build sqlite — or any networked
engine — until the existing json implementation has been moved behind this
API.** The premise of this design is that the old abstraction was drawn on
the wrong axis; the cheapest possible test of that claim is to express the
CURRENT implementation through the new interface. If json cannot sit behind
`Scope`/`Collection`/`CommitMode` cleanly, the boundary is wrong and we learn
it before a line of sqlite exists. Cost of this ordering, stated plainly: P0
and P1 deliver ZERO performance improvement — json is O(records) per write by
contract — and the measured win arrives only in P2.

- **P0 — boundary only, semantics unchanged (releasable, zero user-visible
  change).** The `meta` package, its contract-test suite (including the
  forced-retry wrapper, which is how retry-safety is enforced even though the
  json engine never retries), and ONE engine: today's json implementation
  moved inside it — same files, same format, same `.prev` recovery, same
  flocks. Every subsystem's index moves onto Collections and domain
  repositories. `storage.Store[T]` and `storage/json` retire at the end of
  P0, since nothing is left on them.
  Gate: byte-identical golden fixtures for every namespace file, the full
  contract suite green, `.prev` recovery and crash-boundary tests in the real
  write order, and the paired-ratio non-regression gate. A fixture mismatch
  here is unambiguous — it can only mean the boundary moved something it
  should not have.
- **P1 — protocol changes, still json-only (releasable).** Everything that
  alters behavior rather than structure: entity lock relocation out of
  cleanup sets plus identity revalidation, the tombstone phase protocol with
  payloads, `...Locked` entrypoints, every destructive flow (not just GC) on
  the protocol, the `vm rm` network-teardown restructure, and removal of
  `gc.Module.Locker` / `Watchable.WatchPath` / `IndexFile`/`IndexLock`. The
  GC design gets validated for real, on the engine we already trust.
  Gate: the correctness block of §9 in full — lock-inode safety, tombstone
  fencing/ABA, phase-directed recovery, commit atomicity under crash,
  multi-process correctness.
- **P2 — the sqlite engine (opt-in) and everything that exists only for it.**
  Schema, runtime contract, `meta init`/`meta convert` with its manifest, the
  per-engine event tokens, `meta_state`. Scale deployments convert ALL
  namespaces in one downtime window (a partial migration splits the
  cross-reference graph, §5). This is where the §9 performance gates and the
  absolute anchors apply, and where the measured win lands.
- **P3 — optional follow-ons**: metering `Log`; a networked engine if the
  fleet ever wants one; widening the Relaxed set per operation with data.
- Revertibility: P0 and P1 are ordinary code changes on one engine, revertible
  by revert. P2's conversion is reversible only through
  `meta convert --to json`.
