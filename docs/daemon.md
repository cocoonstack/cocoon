# Daemon

`cocoon daemon` is an optional resident supervisor for VMs the CLI created. Every
CLI verb keeps working with no daemon running; when one is running it adopts the
live VMM processes, notices exits as they happen, and converges metadata and host
networking instead of waiting for the next command to heal them.

## What it fixes

Without a supervisor, a VMM that exits outside `cocoon vm stop` leaves work
undone until something touches the VM again:

- the tc redirect on its now-idle TAP keeps storming softirqs, the condition
  behind the outage fixed for the normal stop path in #130;
- the compute interval stays open, so the metered stop time drifts;
- the record still reads `running`, and `list` can only render `stopped (stale)`;
- consumers that want lifecycle signals have to poll by exec'ing the CLI.

## Running it

```bash
cocoon daemon                          # adopt everything, reconcile every 5s
cocoon daemon --reconcile-interval 2s  # tighter full-pass floor
cocoon daemon --gc-interval 1h         # also sweep periodically (off by default)
cocoon daemon --no-api                 # supervision only, no socket
```

One instance per root directory: a second one fails fast on
`<root-dir>/locks/daemon.lock` without touching any VM.

### systemd

```ini
[Unit]
Description=Cocoon VM supervisor
After=network.target

[Service]
ExecStart=/usr/local/bin/cocoon daemon
Restart=always
RestartSec=1
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
```

Shutdown is clean: VMs keep running, and the next start re-adopts them.

One operational note: the daemon is the first long-lived writer of the metering
ledger, and the `file` backend holds that file open for its whole lifetime. If
you rotate `<root-dir>/metering/ledger.jsonl` out from under it, the daemon keeps
writing to the unlinked inode until it restarts. Either restart the daemon after
a rotation, or run the `meta` metering backend instead.

## How supervision works

Each pass — on a meta change event, or on the reconcile ticker — reads every
record once and applies the same idempotent rules:

| Record | Process | Action |
|---|---|---|
| `creating` | any | ownerless only if the ops lock is free; then run the stale-create cleanup at once, freeing the name |
| `running` | live | watch that exact process generation |
| committed, not `running` | live | repair to `running` under the lock, then watch it |
| any | dead | close the interval, mark `unexpected-exit`, quiesce the network |
| `quiesce_pending` | dead | retry the quiesce |
| unfinished delete tombstone | any | finish the delete — supervision starts deletes of its own, so it resumes them |

Exits are detected with `pidfd` + `epoll`, so the normal path costs no polling;
the ticker is the correctness floor for anything the watcher missed, including
exits that happened while the daemon was down.

Every mutation takes the same per-VM ops lock the CLI takes. A busy lock simply
defers the work to the next pass, so supervision never blocks a command and never
interleaves with one.

### What it will not do

The daemon reports an unexpected exit; it never restarts a VM. Restart is policy
and belongs to whatever runs on top of cocoon. It also refuses to guess a cause:
an adopted process cannot report why it died, so every exit outside a cocoon stop
is recorded as `unexpected-exit`.

If you do build a restart policy on top, trigger it on the `unexpected-exit`
transition rather than on your own kill. `cocoon vm start` issued in the same
instant a VMM is killed races that VMM's teardown and fails its launch timeout —
with or without a daemon. Waiting for the transition costs about 30ms and makes
the restart reliable.

## Transition tracking

Supervision needs to tell a transition it has seen from one it has not, so every
committed state change stamps the record:

- `transition_generation` — increments once per committed transition;
- `last_transition_reason` — `create`, `boot`, `restart`, `clone`, `restore`,
  `stop-user`, `error`, or `unexpected-exit`;
- `last_transition_at`.

`cocoon vm inspect` shows all three.

For an exit the watcher saw, `stopped_at` is the moment it became readable. For
one discovered by a scan — during daemon downtime, say — it is detection time:
the daemon does not claim an exit time it cannot know.

## Read-only API

The daemon serves HTTP/JSON over `<run-dir>/cocoond.sock` (`0660`, in a `0750`
directory — filesystem permissions are the authorization boundary).

```bash
curl --unix-socket /var/lib/cocoon/run/cocoond.sock http://localhost/v1/vms
curl --unix-socket /var/lib/cocoon/run/cocoond.sock -N http://localhost/v1/events
```

- `GET /healthz` — 200 while the loop is running and its last pass is recent;
  503 only when a backend could not be scanned at all or the loop has stalled,
  since that is the case where restarting the daemon might help. VMs the pass
  could not converge are reported as a `degraded` count instead — a single VM
  nobody can converge must not put a working daemon into a restart loop.
- `GET /v1/vms` — every supervised VM: persisted state, transition triple, current
  liveness, watched pid.
- `GET /v1/events` — SSE. Opens with a `sync` snapshot, then streams `ADDED`,
  `MODIFIED` and `DELETED` changes.

The stream is a hint, not a log: it replays nothing and a slow reader loses
events. After a reconnect, compare each VM's `transition_generation` against the
last one you handled — a generation you have not seen whose state is `stopped`
and whose reason is `unexpected-exit` is a crash you missed.
