# Known Issues

Limitations, workarounds, and upstream tracking. Fixed entries are kept for operators running older binaries.

## Clone networking needs in-guest reconfiguration

`cocoon vm clone` allocates the clone its own IP, but the guest still carries the source VM's network configuration, so the clone does not answer on that IP until the guest is reconfigured. What the guest wakes up with differs by backend, because only Cloud Hypervisor changes the guest-visible MAC:

- **Cloud Hypervisor** hot-swaps the snapshot's NICs while the VM is paused, so the clone comes up on a new interface with a fresh MAC (`ens3` → `ens8` on cloudimg, `eth0` → `eth1` on OCI). The snapshot's MAC-matched configuration then matches nothing and the guest has **no IPv4 address at all**: it cannot reach the network, and it cannot collide with the source VM either.
- **Firecracker** keeps the source VM's guest MAC in the restored vmstate (see [Firecracker clone guest MAC address](#firecracker-clone-guest-mac-address)), so the per-MAC configuration still matches and the guest wakes up holding **the source VM's IP**.

**Consequence on Firecracker**: if the source VM is still running, both VMs advertise the same IP with different MACs, the upstream gateway flaps between them, and both see **intermittent connectivity loss** until the clone is reconfigured. Nothing in the data path prevents this — it is L2 end to end (TC ingress redirect + bridge), no component checks the guest's source IP against the allocated one, and bridge CNI does not enforce IP ↔ veth binding (`host-local` IPAM only tracks allocations in its control-plane state files, not in data-plane rules).

**Mitigation**: run the post-clone guest setup commands printed by `cocoon vm clone` (see [Post-Clone Guest Setup](snapshots.md#post-clone-guest-setup)). For cloudimg VMs they re-run `cloud-init`; for OCI VMs they replace the generated `/etc/systemd/network/10-*.network` files with per-MAC units for the new IP and restart `systemd-networkd`; on Firecracker they set the new MAC first. The clone answers on its allocated IP a few seconds after they run.

## OCI VM multi-NIC kernel IP limitation

OCI VMs get one `ip=` boot parameter per NIC. The initramfs-tools `init` script keeps only the last `ip=`, so the initramfs configures only the last NIC and writes a `.network` file only for it. After switch_root, systemd-network-generator writes a `70-<nic>.network` file for every `ip=`, but the images ship `/etc/systemd/network/20-wired.network` (`Name=e* v*`, `DHCP=yes`), which sorts first and wins.

**Consequence**: on every boot of an OCI VM with multiple NICs, the first `vm run` included, only the last NIC gets its static IPv4 address. The other NICs ask for DHCP and, on a network without a DHCP server, have no IPv4 address. Removing a NIC with `vm net` while the guest is still in its initramfs makes that boot wait for the missing interface: about 13 minutes on the Ubuntu images.

**Workaround**: persistent MAC-based systemd-networkd configs for **all** NICs configure every interface on every boot. The post-clone setup hints write them for a clone; a VM created with `vm run --nics N` has only the one the initramfs writes for the NIC its `ip=` configured, so write one `.network` file per other NIC in the guest, matching the MAC and address `cocoon vm inspect` reports. Renaming `20-wired.network` in the guest so it sorts after `70-` also lets the generated files apply. Run `vm net` after the guest has booted.

## Non-root user creation requires cloud-init final stage

When `--user` specifies a non-root username (e.g. `--user admin`), the user is created via cloud-init `runcmd` which runs in the **final stage** — after networking, SSH key generation, and other modules. This means:

- The user does not exist until cloud-init fully completes (typically 20-30s after boot)
- SSH login as the custom user will fail if attempted before cloud-init finishes
- `cloud-init status` shows `done` when the user is ready

The root user's password is set via `chpasswd` (config stage, earlier) and is available sooner, but `--user admin` deliberately does not set a root password — root stays locked for security.

**Workaround**: wait for `cloud-init status: done` before attempting SSH. The default `root`/`cocoon` credentials use the faster `chpasswd` path and work on the console as soon as it is set; stock cloud images ship sshd with `PermitRootLogin prohibit-password`, so root over SSH needs a key or a custom image — use `--user` for password SSH.

## Clone preserves guest credentials from snapshot

Clone regenerates cidata for **network reconfiguration only** — it does not inject new user or password settings. The cloned VM's credentials are whatever the source VM had in `/etc/shadow` at snapshot time.

- `--user`/`--password` flags are not available on `cocoon vm clone`
- If you need different credentials, change them inside the guest after boot

This is by design: clone restores the VM's exact state including all account settings.

## `vm exec` requires vsock — legacy VMs excluded

`cocoon vm exec` dials the cocoon-agent inside the guest over hybrid vsock. Three caveats:

- **Legacy VMs (created before vsock support landed)** have no vsock UDS bound. `vm inspect` omits `vsock_socket` and `vm exec` returns `vsock not configured for this VM (recreate the VM to enable agent exec)`. Recreate the VM to gain exec capability.
- **FC clone vsock requires FC ≥ v1.16** (the `vsock_override` field on `PUT /snapshot/load` was merged post-v1.15). Older FC rejects the field with `unknown field vsock_override`. Cocoon omits the field on same-VM restore, so FC < v1.16 still runs and restores fine — only `vm clone` needs the field.
- **FC v1.16.0 snapshot-restore permanently breaks guest vsock** ([#140](https://github.com/cocoonstack/cocoon/issues/140)): after `vm clone` or `vm restore` the guest agent never answers hybrid-vsock `CONNECT`, with no recovery. Fixed upstream in FC v1.16.1 — the effective floor for any FC restore/clone lane. `doctor/check.sh` installs the fork's rolling `dev` release, which carries the fix.

Windows guests are supported as of cocoon-agent v0.1.3 (registered via SCM, runs as `LocalSystem`). Official `ghcr.io/cocoonstack/windows/win11:*` images bake the agent and a `CocoonNicAutoHeal` scheduled task that recovers chained-clone NDIS-stuck NICs in-guest. DIY Windows images need to install both pieces themselves; without the agent, `vm exec` returns `read CONNECT reply: EOF`.

Race window: `cocoon vm run X && cocoon vm exec X -- cmd` may fail with `read CONNECT reply: EOF` if the in-guest agent hasn't started yet. The error includes the hint `(cocoon-agent may still be starting; retry shortly)`. Wait ~5–10s after `vm run` returns.

## Cloud image UEFI boot compatibility

On x86_64, cocoon uses [rust-hypervisor-firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware) (`CLOUDHV.fd`) for cloud image UEFI boot. This firmware implements a minimal EFI specification and does **not** support the `InstallMultipleProtocolInterfaces()` call required by newer distributions. aarch64 hosts boot through EDK2 (the `CLOUDHV_EFI.fd` release asset, which `cocoon-check --upgrade` installs as `CLOUDHV.fd`), which is not affected.

**Affected images** (kernel panic on boot — GRUB loads kernel but not initrd):

- Ubuntu 24.04 (Noble) and later
- Debian 13 (Trixie) and later

**Working images**:

- Ubuntu 22.04 (Jammy)

This is an upstream issue tracked in [rust-hypervisor-firmware#333](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/issues/333) and [cloud-hypervisor#7356](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7356). As a workaround, use **OCI VM images** for Ubuntu 24.04 — OCI images use direct kernel boot and are not affected.

## Data disk fsck sends an existing VM to the emergency shell

Cidata from v0.7.6 and earlier gives a mounted data disk fstab pass number 2. If the host's `mke2fs` formats with a feature the guest's `e2fsck` does not know, such as `orphan_file` from a Debian 13 host with an Ubuntu 22.04 guest, the next boot fails the fsck and enters the emergency shell with no network and no sshd, despite `nofail`. New cidata writes pass number 0.

**Recovery**: log in through `cocoon vm console`, set the last field of the disk's `/etc/fstab` line to 0, then stop and start the VM.

## Named netns path removed while a VM runs

cocoon never removes a running VM's named network namespace. If an operator or a foreign GC runs `ip netns del`, it removes the name under `/var/run/netns`; the running VMM still holds the original namespace and its TAPs. Existing traffic may continue, and `vm list` still shows the recorded IP. NIC hot-add cannot open the missing netns path, while `cocoon vm exec` can still work over vsock.

To restore the named path, stop and relaunch the VMM through the start-time recovery path (`Verify` → `Prepare` → re-plumb every NIC with its recorded MAC, IP and TAP name):

- keep guest memory: `cocoon vm hibernate VM` then `cocoon vm restore VM <snapshot>` — about 300 ms, processes and RAM untouched
- or reboot the guest: `cocoon vm stop --force VM` then `cocoon vm start VM`

Do not recreate the name by hand while the VM runs: it would point to a different namespace from the one holding the VMM and existing TAPs, so a later NIC hot-add would provision in the wrong place.

## DHCP networks should not use DHCP IPAM in CNI

When using a DHCP-based network (e.g., macvlan attached to a network with an external DHCP server), the CNI conflist should **not** use the `dhcp` IPAM plugin. Instead, configure the CNI plugin with **no IPAM** (or `"ipam": {}`) and let the guest obtain its IP directly from the external DHCP server.

The `dhcp` IPAM plugin runs a host-side DHCP client that competes with the guest's own DHCP client, causing:

- **Duplicate DHCP requests** — both host-side (CNI IPAM) and guest-side DHCP clients request leases for the same MAC, confusing DHCP servers and leading to lease conflicts.
- **IP mismatch** — the host-side DHCP client may obtain a different IP than the guest, so Cocoon's recorded IP does not match the guest's actual IP.
- **Lease renewal failures** — the CNI `dhcp` daemon must remain running to renew leases; if it crashes or is restarted, the host-side lease expires while the guest keeps using the IP.

This applies to **all CNI plugins** where the upstream network provides DHCP (bridge with external DHCP, macvlan, ipvlan, etc.). The correct approach is:

```json
{
  "cniVersion": "1.0.0",
  "name": "my-dhcp-network",
  "plugins": [
    {
      "type": "macvlan",
      "master": "eth0",
      "mode": "bridge",
      "ipam": {}
    }
  ]
}
```

Cocoon detects when CNI returns no IP allocation and automatically configures the guest for DHCP — cloudimg VMs get `dhcp4: true` in their Netplan network-config (plus MAC-matched `DHCP=ipv4` systemd-networkd units written by cloud-init), and OCI VMs get DHCP systemd-networkd units generated by the initramfs `cocoon-network` script.

Note: the OCI initramfs uses `IP=off` to prevent the initramfs from running its own DHCP client during boot. DHCP is handled entirely by systemd-networkd after switch_root. The `configure_networking` function is only called when a kernel `ip=` parameter is present (static IP from CNI).

## Bridge plugin `ipMasq` leaks a masquerade rule per VM

With `"ipMasq": true`, the CNI bridge plugin adds a POSTROUTING rule for each VM address and a `CNI-…` chain for each VM on ADD. On DEL it removes them only for the addresses it reads off the VM's interface in the netns. Cocoon gives that address to the guest and removes it from the netns interface when it sets up the TC redirect, so the plugin finds none, and every removed VM keeps its rule and chain. The plugin's iptables backend also shares one chain across all NICs of a VM on one network, so a teardown of one NIC would clear it under the others.

Leave `ipMasq` off and masquerade the bridge subnet on the host once:

```bash
iptables -t nat -A POSTROUTING -s 10.22.0.0/16 ! -o cni0 -j MASQUERADE
```

The rules an earlier `ipMasq` network left carry its name in their comment, for a network named `cocoon`:

```bash
iptables-save -t nat | grep -F 'name: \"cocoon\"'
```

## Windows VM does not respond to ACPI power-button

**Status: FIXED** in our firmware fork.

Cloud Hypervisor uses a GED (Generic Event Device, `ACPI0013`) to deliver power-button notifications on its hardware-reduced ACPI platform. While this mechanism works correctly for Linux guests, Windows guests did not respond to the `vm.power-button` API call — no power-button event appeared in the Windows event log (`Event ID 109`).

**Root cause**: the EFI `ResetSystem` runtime service in [rust-hypervisor-firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware) was a no-op. When Windows attempted a graceful shutdown via the UEFI reset path, nothing happened. Tracked in [cloud-hypervisor/rust-hypervisor-firmware#422](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/issues/422) and [cloud-hypervisor/cloud-hypervisor#7929](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7929).

**Fix**: our firmware fork ([cocoonstack/rust-hypervisor-firmware `dev` branch](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev)) implements `ResetSystem` properly. Upstream merged the same fix in [cloud-hypervisor/rust-hypervisor-firmware#424](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/pull/424) (replacing #423); no firmware release carries it yet, the latest being 0.5.0. With this fix, the ACPI power-button works for Windows guests, and `cocoon vm stop` completes in ~8-13 seconds on a fully booted VM.

**Timing caveat**: the Windows ACPI shutdown handler needs ~60 seconds from cold boot to fully initialize (SAC appears at ~30s, handler ready ~30s later). Stopping a Windows VM before the handler is ready triggers the 30s `stop_timeout_seconds` fallback and escalates to force-kill. Clone-restored VMs inherit the ready ACPI state and shut down in ~8-13s immediately.

**Background servicing**: after about 15 minutes of uptime, Windows runs maintenance such as `StartComponentCleanup`, and shutdown waits for `TrustedInstaller` to stop. In testing a cold-booted VM took 20s and a clone 73s to power off, with the power-button event logged at once, so `vm stop` can reach the 30s `stop_timeout_seconds` and force-kill. Raise `stop_timeout_seconds` for a Windows VM that must stop cleanly.

**Previous consequence** (no longer applies with our firmware fork): `cocoon vm stop` always timed out on Windows VMs (default 30s), then fell back to `vm.shutdown` → SIGTERM → SIGKILL.

**Previous workaround** (no longer needed with our firmware fork): shut down Windows guests via SSH or WinRM before stopping:

```bash
ssh cocoon@<vm-ip> "shutdown /s /t 0"
cocoon vm stop <vm>
```

Or use `cocoon vm stop --force` to skip the ACPI timeout and immediately kill the process.

The Windows image's `autounattend.xml` includes defensive power-button configuration (`PBUTTONACTION=3`) and shutdown optimization (`WaitToKillServiceTimeout=5000`, `shutdownwithoutlogon=1`) which remain useful for environments not using our firmware fork.

## Windows VM balloon disabled

**Status: WONTFIX** — disabled by design.

Cocoon does not attach a virtio-balloon device to Windows VMs (`--windows`). The virtio-win balloon driver >= 0.1.262 ([PR #1157](https://github.com/virtio-win/kvm-guest-drivers-windows/pull/1157), "cope with the unresponsive host") changed balloon deflation behavior: when the host does not ACK a balloon page-batch within 1 second, the driver now **retries indefinitely** instead of giving up. This causes two problems during ACPI power-button shutdown:

1. **Deflation CPU storm**: Windows begins tearing down virtio devices during shutdown. The balloon driver attempts to deflate (return all reclaimed pages to the guest) one 512-page batch at a time, retrying on every host timeout. This pins all vCPUs at 100% for 2–3 minutes ([virtio-win #1148](https://github.com/virtio-win/kvm-guest-drivers-windows/issues/1148), open, unresolved upstream).

2. **Watchdog reboot**: Cloud Hypervisor's virtio-watchdog has a 20-second timeout. Windows stops petting the watchdog during shutdown, and the balloon deflation outlasts the timeout — the watchdog triggers a VM reset before Windows can call UEFI `ResetSystem` to write to the ACPI shutdown port (0x600). The VM reboots instead of powering off.

virtio-win 0.1.240 did not have this problem because its balloon driver gave up on the first host timeout, allowing Windows to proceed to `ResetSystem` quickly (~14 seconds total shutdown).

**Workaround if balloon is needed**: increase `stop_timeout_seconds` to 180+ and apply the watchdog-pause patch to Cloud Hypervisor (pause the watchdog timer when `vm.power-button` is received).

**Workaround for guest-initiated reboot**: if the guest driver leaves the watchdog armed, create the VM with `--no-watchdog`. This trades device-level hang reset for reboot safety and should be paired with an external liveness policy where automatic recovery is required.

## Installing patched binaries for Windows

See [cocoonstack/windows](https://github.com/cocoonstack/windows) for download and installation instructions.


## Firecracker snapshot portability

Firecracker snapshots store absolute host paths in the vmstate binary (Rust serde format, not patchable). This means:

- **Same-host clone/restore**: restore sends no drive override, so the vmstate reopens the paths it recorded; the restore preflight therefore refuses a snapshot whose COW or data disks are recorded under another VM's run dir (even with `--force`), and clone rejects a missing source drive whose recorded path falls outside the current managed `run_dir`
- **Cross-host export/import**: requires the target host to use **identical `root_dir` and `run_dir`** (default: `/var/lib/cocoon` and `/var/lib/cocoon/run`) and have the **same OCI image pulled**
- **Drive path redirect**: during `snapshot/load` Cocoon bind-mounts the clone's disks over the source-absolute paths inside a private mount namespace, holding a shared per-VM lease on the source so concurrent source operations wait; a dead source gets an empty placeholder file to bind over. The host namespace is never mutated

This is a fundamental Firecracker design limitation. Cloud Hypervisor snapshots do not have this restriction because CH stores device config in a patchable JSON format (`config.json`).

**Upstream fix in progress**: Firecracker [PR #5774](https://github.com/firecracker-microvm/firecracker/pull/5774) adds `drive_overrides` to the `PUT /snapshot/load` API, which would eliminate the bind-mount redirect and make FC snapshots natively portable. Track this PR for future simplification.

## Firecracker virtio-blk serial numbers

Firecracker does not support virtio-blk serial numbers. Cocoon's OCI init script (`overlay.sh`) uses device paths (`/dev/vdX`) instead of serial names to identify disks when booting under Firecracker. OCI images built from `os-image/ubuntu/overlay.sh` (v0.3+) support both formats automatically. Older images must be rebuilt to work with `--fc`.

## Cloudimg post-restart snapshot includes orphan `cidata.img`

A cloudimg VM snapshotted after a stop/start carries an unreferenced ~1 MB sparse `cidata.img`; clone overwrites it and restore keeps it. Harmless.

## Firecracker clone guest MAC address

Firecracker does not support overriding the guest MAC address during snapshot/load. Cloned FC VMs retain the source VM's guest MAC (baked into the vmstate binary). In Cocoon's TC redirect architecture, each VM runs in an isolated network namespace, so MAC identity is not visible to other VMs or the host bridge — **no MAC conflict occurs in practice**.

On CNI plugins with strict per-veth MAC enforcement (Cilium eBPF, Calico eBPF), the guest MAC vs veth MAC mismatch could theoretically cause packet drops. This has not been observed in testing with the standard bridge CNI.

**Upstream status**: FC's `NetworkOverride` struct only has `iface_id` and `host_dev_name` — no `guest_mac` field. Adding it would follow the existing `VsockOverride` pattern. No issue or PR exists yet.

**Workaround**: If MAC matching is required, run `ip link set dev ethX address <new-mac>` inside the guest after clone (the post-clone hints print the expected MAC values).

## Vhost-user-fs requires VM-level shared memory

`cocoon vm fs attach` only works on CH VMs that were created with `--shared-memory`. CH's `memory shared=on` is fixed at VM creation: backend processes (e.g. virtiofsd) need to mmap guest memory via the negotiated memfd, and the memory model cannot be flipped on a running VM. If `--shared-memory` was omitted at create time, the only path is to recreate the VM. Cocoon's preflight reads `vm.info` and surfaces a clear error rather than letting CH return a vague rejection.

## A killed capture leaves the guest paused

`snapshot save` and `vm hibernate` pause the guest for the capture. A canceled or timed-out capture attempts to resume the guest with a separate 10 minute timeout. If the cocoon process dies or that resume fails, the guest can remain paused: the VM record reads `running`, the guest is frozen, and `vm exec` hangs. SIGKILL does this. SIGPIPE does too: when the process that reads cocoon's stderr exits, cocoon dies on its next log line, and for a hibernate that line comes after the snapshot persists and before the VMM stops.

`cocoon vm start VM` resumes it. The hypervisor API does not answer during a dump, so until the dump ends the command fails after 30 s; retry it. `vm inspect` also waits on that API, up to 30 s per device list, then prints without the attached devices; `vm list` does not call it.

A killed capture also leaves its files. A `snapshot-*` directory under the VM's run dir stays until `vm rm` or a `gc` 24 hours later. A hibernate killed while it persists leaves a hidden pending snapshot that keeps its `--name` taken until the next `gc`. A hibernate killed after it persists leaves a listed snapshot that keeps its `--name` until `snapshot rm`.

## Windows: a capture right after a clone can strand the next clone's NIC

A clone's eject ack does not mean its replacement NIC has started: netkvm finishes starting 0.2-1.8 seconds later. A snapshot taken in between yields a clone whose own eject never acks. Its next capture then waits the full 30 seconds and warns, and a clone of that snapshot has no new NIC while `vm clone` reports success. On Windows, a capture that finds an eject pending therefore also waits, inside the same 30 seconds, until every NIC has sent a frame, and it re-sends `vm.remove-device` every 5 seconds for an eject the guest dropped. A capture started in the short window after the ack and before the NIC starts is not covered: wait a few seconds after cloning a Windows VM before capturing it.

A NIC in Error code 10 (netkvm event 5005) at capture strands the eject the same way, with any delay. It appears when Windows re-enables several NICs at once: it moves a NIC's BAR into a range Cloud Hypervisor still assigns to another disabled NIC, Cloud Hypervisor refuses and logs `Failed moving device BAR ... keeping old BAR`, and the driver starts on resources that do not match. Disable and re-enable NICs one at a time; the clone hint and the image's `CocoonNicAutoHeal` task do.

## A serial console slows NIC bring-up and VM wake

On a host whose kernel console is a serial port (`console=ttyS0,115200`) at console_loglevel 7, every bridge port state change prints `entered blocking state` and `entered forwarding state` synchronously, about 6.5 ms per line, while the kernel holds RTNL. A VM wake or start pays about 14 ms of it, and any other netlink or TAP operation on the host waits behind it. Lowering console_loglevel (the first field of `/proc/sys/kernel/printk`) to 4 cuts the link-up work from 13.6 ms to 0.4 ms; the lines still reach `dmesg`.

## Snapshot lease files are never reclaimed (fixed in v0.6.8)

Fixed in v0.6.8 by #253: `snapshot rm` unlinks the snapshot's `<root>/snapshot/localfile/<id>.lease` when it releases the exclusive lease, and `gc` reclaims the leases of snapshots that no longer have a record or a data dir, so a host upgraded from an older binary converges on its first `gc` run (133 leftover files on one testbed went to the four live snapshots' leases).

On v0.6.7 and older every `snapshot save` leaves that zero-byte file behind and `snapshot rm` and `gc` remove only the data dir, so the files accumulate one inode per snapshot ever created; delete them by hand while no snapshot verb runs.

## `--from-dir` and `snapshot import` reject a snapshot rebuilt by a third-party tar

`cocoon vm clone --from-dir`, `cocoon vm restore --from-dir` and `cocoon snapshot import` compare `cow.raw`'s on-disk size with the size recorded in `snapshot.json`. cocoon's export tar stores sparse disks with private pax records that only its own extractor understands; a generic `tar -x` drops them and rebuilds a short, shifted disk that would restore into silent corruption. The snapshot is refused with `cow.raw in <dir> is N bytes but the envelope records M`. Use `snapshot export --to-dir` for the directory form, or `snapshot import` on the tar exactly as cocoon wrote it.

## virtiofsd is a single-shot daemon

Upstream virtiofsd serves exactly one vhost-user client and exits when that client disconnects. Consequence: after `cocoon vm fs detach`, the daemon is gone — a follow-up `cocoon vm fs attach` against the same socket path will hang or time out until a fresh `virtiofsd` instance is launched. The same applies after `cocoon vm stop` (CH closes the socket on shutdown). Scripts that cycle attach/detach should respawn virtiofsd between calls. This is a virtiofsd behavior, not a cocoon limitation.

## Official OS images ship with default `root:cocoon` and `PermitRootLogin yes`

Every Ubuntu and Debian image under `os-image/` enables `openssh-server` with `PermitRootLogin yes` and the default `root:cocoon` credentials baked in. This is convenient for development and matches the existing OCI-image behavior, but it is **not** safe for production exposure.

Mitigations for production users:

- Rotate the root password (`passwd root`) and/or disable password auth (`PasswordAuthentication no`) inside the guest before exposing it.
- Add a non-root sudo user, then flip `PermitRootLogin` back to `no`.
- Or fork `install-agent.sh` and drop its sshd step (the `PermitRootLogin` sed and `systemctl enable ssh`); the script has no flag or env knob for it.

Control-plane traffic from cocoon-managed hosts (vk-cocoon, `cocoon vm exec`) goes through cocoon-agent over vsock and never depends on SSH credentials.

## NIC hot-remove leaves the PCI slot pending on Cloud Hypervisor

`cocoon vm net --nics N` waits for CH's `device_tree` to drop the removed device (polled via `vm.info` until the entry disappears, max 30 s) before tearing down host plumbing. CH only frees the PCI slot — and unregisters the device's ioeventfds — when the guest writes to the ACPI hot-plug controller (B0EJ) in response to the SCI raised by `remove-device`. If the guest never ACKs (driver wedged, paused, NDIS halted), the wait times out, the cocoon record is left intact, and the command returns an error so the user can quiesce the guest and retry.

If the host plumbing teardown itself fails after a successful eject, cocoon truncates the record anyway and surfaces a warning — the orphan TAP / veth / CNI lease is cleaned up by `cocoon vm rm` or the next gc cycle. The user still has to consider:

- The guest may continue to reference a NIC whose host plumbing is gone, hanging the in-guest driver.
- The pending eject may surface later as `Cannot register ioevent: File exists` when CH next tries to reuse that slot (e.g. a subsequent hot-add).

Quiesce the guest NIC (ip link set down + NetworkManager remove on Linux; Disable-PnpDevice + driver unbind on Windows) before reducing the count.

Firecracker VMs created with `--pci` resize through the same command. After removing a NIC, cocoon polls `/vm/config` for up to 30 s for the VMM to drop it and returns the same eject-pending error if it does not; it also prints rescan/remove steps for builds that plug devices onto the root bus (see [networking](networking.md#nic-hot-resize-cloud-hypervisor-or-firecracker-with---pci)). MMIO Firecracker VMs are rejected.

## NIC hot-remove during clone hot-swap cannot wait for guest B0EJ

Plain `cocoon vm net --nics N` polls CH's `device_tree` after `vm.remove-device` until the guest ACKs B0EJ via ACPI/SCI (typically < 1 s on Linux, up to 14 s on Windows). The clone path's `hotSwapNets` runs while the VM is paused, so the guest cannot process SCI — eject stays pending until resume. The clone path therefore returns without waiting and adds the fresh NICs against half-removed device-tree state. This is the long-standing CH limitation cocoon was designed around; the wait only applies to the running-VM resize.

## Android cocoon-agent service may be blocked by SELinux

`os-image/android/{14.0,15.0,15.0-gms}` install the cocoon-agent binary at `/system/bin/cocoon-agent` and register it via `/system/etc/init/cocoon-agent.rc`. The official images boot with `androidboot.selinux=permissive` (`os-image/android/init-wrapper.sh`), so the service runs and SELinux denials are only logged. On an enforcing build the policy has no domain for cocoon-agent and may deny it outright: if `cocoon vm exec` returns `dial agent: ...`, check `logcat | grep -i avc` inside the guest and grant the binary network/socket permissions in the Android sepolicy at build time.
