#!/usr/bin/env python3
"""Bring a cocoon root to meta schema generation 2, once, after the binary swap.

Generation 2 adds a placements table per VM namespace: one row per placed VM
holding the host cpus its queue threads pin to, mirrored by every record write.
A cocoon at generation 2 refuses an older sqlite store by name; this script is
the upgrade. On a sqlite root it creates the tables, writes one row for every
record that carries a placement (VMs the previous binary launched and left
running), and stamps the generation, in one transaction. On a json root it adds
the same rows to each VM namespace file and its .prev generation under the
engine's lock. Rerunning is a no-op. Run it as the user that owns the root, with
the old binary already gone.

usage: meta-upgrade.py [--root-dir /var/lib/cocoon]
"""

import argparse
import fcntl
import json
import os
import sqlite3
import sys

APPLICATION_ID = 0x434F434E
USER_VERSION = 2
BACKENDS = ("cloudhypervisor", "firecracker")


def placement(record):
    return record.get("queue_cpus") or record.get("cpuset") or None


def upgrade_sqlite(db_path):
    if os.path.exists(os.path.join(os.path.dirname(db_path), "meta-convert.manifest")):
        sys.exit(f"{db_path}: a meta convert is in flight; finish it first")
    db = sqlite3.connect(db_path, timeout=5, isolation_level=None)
    app_id = db.execute("PRAGMA application_id").fetchone()[0]
    if app_id != APPLICATION_ID:
        sys.exit(f"{db_path}: application_id {app_id:#x} is not a cocoon meta store")
    version = db.execute("PRAGMA user_version").fetchone()[0]
    if version > USER_VERSION:
        sys.exit(f"{db_path}: schema version {version} is newer than this script ({USER_VERSION})")
    db.execute("BEGIN IMMEDIATE")
    for backend in BACKENDS:
        ns = f"vms_{backend}"
        table = f'"{ns}__placements"'
        db.execute(f"CREATE TABLE IF NOT EXISTS {table} (id TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL)")
        rows = 0
        for vm_id, data in db.execute(f'SELECT id, data FROM "{ns}__records"').fetchall():
            cpus = placement(json.loads(data))
            if cpus:
                db.execute(f"INSERT OR REPLACE INTO {table} (id, data) VALUES (?, ?)", (vm_id, json.dumps(cpus).encode()))
                rows += 1
        print(f"{ns}: {rows} placement rows")
    db.execute(f"PRAGMA user_version = {USER_VERSION}")
    db.execute("COMMIT")
    db.close()
    print(f"{db_path}: schema generation {version} -> {USER_VERSION}")


def write_file(path, data):
    tmp = path + ".upgrade"
    with open(tmp, "w") as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    os.rename(tmp, path)


def upgrade_json(root):
    found = 0
    for backend in BACKENDS:
        db_dir = os.path.join(root, backend, "db")
        path = os.path.join(db_dir, "vms.json")
        if not os.path.exists(path):
            continue
        found += 1
        with open(os.path.join(db_dir, "vms.lock"), "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            with open(path) as f:
                doc = json.load(f)
            rows = {vm_id: cpus for vm_id, rec in doc.get("vms", {}).items() if (cpus := placement(rec))}
            if rows == doc.get("placements", {}):
                print(f"{path}: {len(rows)} placement rows already present")
                continue
            doc["placements"] = rows
            data = json.dumps(doc, separators=(",", ":"), ensure_ascii=False) + "\n"
            for target in (path, path + ".prev"):
                write_file(target, data)
            dir_fd = os.open(db_dir, os.O_RDONLY)
            os.fsync(dir_fd)
            os.close(dir_fd)
        print(f"{path}: {len(rows)} placement rows")
    if not found:
        sys.exit(f"{root}: no meta store found (neither meta/meta.db nor a VM namespace file)")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--root-dir", default="/var/lib/cocoon", help="cocoon root_dir (default: %(default)s)")
    root = parser.parse_args().root_dir
    db_path = os.path.join(root, "meta", "meta.db")
    if os.path.exists(db_path):
        upgrade_sqlite(db_path)
    else:
        upgrade_json(root)


if __name__ == "__main__":
    main()
