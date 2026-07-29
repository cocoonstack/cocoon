# Garbage Collection

Cross-module GC, snapshot LRU eviction, and scheduled cleanup.

## How It Works

`cocoon gc` performs cross-module garbage collection:

1. **Lock** all modules (images, VMs, network, snapshots) — if any module is busy, the entire GC cycle is skipped to maintain consistency
2. **Snapshot** all module indexes under lock
3. **Resolve** each module identifies unreferenced resources using the full snapshot set (e.g., image GC checks VM and snapshot records for blob references)
4. **Collect** — delete identified targets

This ensures blobs referenced by running VMs or saved snapshots are never deleted.

### Log Output

Every collected item is logged at INFO level with a structured `key=value` payload under `gc.<module>`, and a summary line ends the cycle. Sample:

```
INFO gc.snapshot          collected id=XEOU... name=ubuntu-hot-testing:v1 bytes=3221225472 last_accessed=2026-04-12T10:30:00Z reason=lru-age
INFO gc.snapshot          collected id=2GQVEA... name= bytes=0 last_accessed=never reason=orphan
INFO gc.cloud-hypervisor  collected id=ABC123 runDir=/var/lib/cocoon/run/cloudhypervisor/ABC123 logDir=/var/log/cocoon/cloudhypervisor/ABC123 reason=orphan-runDir
INFO gc.oci               collected blob=b40150c1c2717d... reason=unreferenced
INFO gc.cni               collected id=JKLMN netns=cocoon-JKLMN nics=2/2 reason=orphan
INFO gc.bridge            collected id=MNOPQ iface=btMNOPQ-0 reason=orphan-tap
INFO gc.Run               completed: cloud-hypervisor=1 cni=1 oci=4 snapshot=3 (failures: 0, duration: 230ms)
```

Filter with `awk` / `grep`:

```bash
journalctl -u cocoon-gc.service --since today | grep "gc.snapshot.*reason=lru-"
journalctl -u cocoon-gc.service --since today | awk '/gc.Run completed/'
```

Reasons:
- **snapshot**: `orphan` (dataDir without DB record), `stale-pending` (a dead save's pending record — its build lease is free), `lru-all` / `lru-age` / `lru-keep` / `lru-size` (multi-criterion uses `+` joiner)
- **cloud-hypervisor / firecracker**: `orphan-runDir`, `orphan-logDir`, `stale-creating`
- **images (oci, cloudimg)**: `unreferenced`
- **cni**: `orphan` (netns without active VM)
- **bridge**: `orphan-tap`
- **vmlock**: `orphan-lease` (lease file for a VM no backend knows)

### Snapshot LRU Eviction

Bare `cocoon gc` only reclaims **orphans** (on-disk data with no DB record) and **stale pending** records (a save died mid-flight; every save holds its snapshot's build lease from placeholder to finalize, so a pending record whose lease GC can acquire is provably ownerless — no age wait). To also evict healthy snapshots by access recency, pass `--snapshot`:

| Flag                 | Effect                                                                                          |
| -------------------- | ----------------------------------------------------------------------------------------------- |
| `--snapshot`           | Enable LRU eviction. Bare flag = evict **every** non-pending snapshot.                          |
| `--snapshot-keep N`    | Keep at most N most-recently-accessed snapshots.                                                |
| `--snapshot-age DUR`   | Evict snapshots last accessed before this duration (e.g. `720h` for 30d).                       |
| `--snapshot-size SZ`   | Evict oldest snapshots until total size ≤ this (e.g. `100GB`).                                  |
| `--snapshot-dry-run`   | Log which snapshots would be LRU-evicted; act on nothing. **Snapshot-only — orphans and other GC modules still execute.** |

Sub-flags combine as union of evictions (intersection of kept) — a snapshot is kept only if it passes **every** active criterion. All sub-flags require `--snapshot`; negative values are rejected.

`LastAccessedAt` is updated on `Restore`, `vm clone` (via `DataDir`), `snapshot export`, and `snapshot import` (set to creation time). `Inspect` and `list` do not count as access.

```bash
# Preview what 30-day eviction would remove (snapshot-only — other GC modules still run)
cocoon gc --snapshot --snapshot-age=720h --snapshot-dry-run

# Production: weekly cleanup, keep 50 newest within 7 days
cocoon gc --snapshot --snapshot-age=168h --snapshot-keep=50

# Cap storage at 100GB
cocoon gc --snapshot --snapshot-size=100GB

# Nuke all snapshots (dev / test reset)
cocoon gc --snapshot
```

### Scheduled Snapshot GC

`cocoon gc` is a one-shot, lock-safe operation — drive periodic execution from a systemd timer or cron. Recommended template (systemd):

```ini
# /etc/systemd/system/cocoon-gc.service
[Unit]
Description=Cocoon snapshot GC (LRU eviction)

[Service]
Type=oneshot
ExecStart=/usr/local/bin/cocoon gc --snapshot --snapshot-age=168h --snapshot-keep=50
StandardOutput=journal
StandardError=journal
```

```ini
# /etc/systemd/system/cocoon-gc.timer
[Unit]
Description=Run cocoon snapshot GC daily

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true
Unit=cocoon-gc.service

[Install]
WantedBy=timers.target
```

Enable: `systemctl enable --now cocoon-gc.timer`.

For cron, drop a one-liner into `/etc/cron.daily/cocoon-gc`:

```sh
#!/bin/sh
exec /usr/local/bin/cocoon gc --snapshot --snapshot-age=168h --snapshot-keep=50
```
