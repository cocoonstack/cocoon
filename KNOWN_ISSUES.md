# Known Issues

## Post-clone IP conflict window

After `cocoon vm clone`, the cloned VM resumes with the **original VM's IP address** configured inside the guest, even though CNI has allocated a new IP for the clone's network namespace. MAC addresses are handled automatically — during clone, the snapshot's old NICs are hot-swapped (removed and re-added with new MACs) while the VM is paused, so the guest wakes up with correct MACs. The clone can still reach the network during the IP conflict window because:

- The entire data path is **L2** (TC ingress redirect + bridge) — no component checks whether the guest's source IP matches the CNI-allocated IP.
- Standard **bridge CNI does not enforce IP ↔ veth binding** at the data plane. The `host-local` IPAM only tracks allocations in its control-plane state files; it does not install data-plane rules.

**Consequence**: if the original VM is still running, both VMs advertise the same IP via ARP with different MACs. The upstream gateway flaps between the two MACs, causing **intermittent connectivity loss for both VMs** until the clone's guest IP is reconfigured.

**Mitigation**: run the post-clone guest setup commands printed by `cocoon vm clone` as soon as possible (see [Post-Clone Guest Setup](README.md#post-clone-guest-setup)). For cloudimg VMs this means re-running `cloud-init`; for OCI VMs this means `ip addr flush` + reconfigure with the new IP.

## Clone resource constraints

Clone resources (CPU, memory, storage, NICs) can only be **increased**, never decreased below the snapshot's original values. See [Clone Constraints](README.md#clone-constraints) for details.

## Restore requires a running VM

`cocoon vm restore` only works on running VMs — it relies on the existing network namespace (netns, tap devices, TC redirect) surviving the CH process restart. A stopped VM's network state may not be intact (e.g., after host reboot the netns is gone). For stopped VMs or cross-VM restore, use `cocoon vm clone` which creates fresh network resources. See [Restore Constraints](README.md#restore-constraints) for all requirements.

## OCI VM multi-NIC kernel IP limitation

OCI VMs use the kernel `ip=` boot parameter for network configuration. While multiple `ip=` parameters can be specified, the Linux kernel only reliably configures **one interface** via this mechanism — subsequent `ip=` parameters may be silently ignored or produce inconsistent results depending on kernel version.

**Consequence**: on a cold boot (stop + start) of an OCI VM with multiple NICs, only the first NIC receives its IP from the kernel `ip=` parameter. Additional NICs must be configured by the guest init system (e.g., systemd-networkd `.network` files written by the post-clone hints).

**Workaround**: the post-clone setup hints write persistent MAC-based systemd-networkd configs for **all** NICs. These survive reboots and correctly configure every interface regardless of the kernel `ip=` limitation.

## Cloud image UEFI boot compatibility

Cocoon uses [rust-hypervisor-firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware) (`CLOUDHV.fd`) for cloud image UEFI boot. This firmware implements a minimal EFI specification and does **not** support the `InstallMultipleProtocolInterfaces()` call required by newer distributions.

**Affected images** (kernel panic on boot — GRUB loads kernel but not initrd):

- Ubuntu 24.04 (Noble) and later
- Debian 13 (Trixie) and later

**Working images**:

- Ubuntu 22.04 (Jammy)

This is an upstream issue tracked in [rust-hypervisor-firmware#333](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/issues/333) and [cloud-hypervisor#7356](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7356). As a workaround, use **OCI VM images** for Ubuntu 24.04 — OCI images use direct kernel boot and are not affected.

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

Cocoon detects when CNI returns no IP allocation and automatically configures the guest for DHCP — cloudimg VMs get `DHCP=ipv4` in their Netplan config, and OCI VMs get DHCP systemd-networkd units generated by the initramfs.

## Windows VM requires Cloud Hypervisor v50.2

**Status: FIXED** in our fork and upstream.

Cloud Hypervisor v51.x had a regression ([#7849](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7849)) that caused Windows to BSOD (`DRIVER_IRQL_NOT_LESS_OR_EQUAL` in `viostor.sys`) when DISCARD/WRITE_ZEROES features were advertised with default-zero config values, violating virtio spec v1.2.

**Fix**: the DISCARD fix is included in our Cloud Hypervisor fork ([cocoonstack/cloud-hypervisor `dev` branch](https://github.com/cocoonstack/cloud-hypervisor/tree/dev)). Upstream has also merged it ([PR #7936](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/7936)). Cloud Hypervisor **v51** now works correctly with Windows VMs.

**Previous recommendation** (no longer needed): use Cloud Hypervisor v50.2 for Windows VMs.

## Windows VM requires virtio-win 0.1.240

**Status: FIXED** in our fork.

virtio-win 0.1.271+ network drivers were incompatible with Cloud Hypervisor due to incomplete virtio-net control queue implementation ([#7925](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7925)). CH only handled `CTRL_MQ` and `CTRL_GUEST_OFFLOADS`; all other commands (`CTRL_RX`, `CTRL_MAC`, `CTRL_VLAN`, `CTRL_ANNOUNCE`) returned `VIRTIO_NET_ERR`.

| Version  | Behavior on VIRTIO_NET_ERR                               |
|----------|----------------------------------------------------------|
| 0.1.240  | Tolerates error, continues working                       |
| 0.1.271  | May silently fail, NIC unusable                          |
| 0.1.285+ | Fail-fast: NdisMRemoveMiniport(), Problem Code 43        |

0.1.285 introduced commit `50e7db9` ("indicate driver error on unexpected CX behavior") with zero-tolerance on control queue errors. Root cause was a CH bug — the correct fix is to return `VIRTIO_NET_OK` for unsupported commands and to report the correct `used_len`.

**Fix**: our Cloud Hypervisor fork includes ctrl_queue command tolerance (from [@liuw](https://github.com/liuw)) plus the `used_len` fix. See [cocoonstack/cloud-hypervisor `fix/virtio-net-ctrl-queue` branch](https://github.com/cocoonstack/cloud-hypervisor/tree/fix/virtio-net-ctrl-queue) (also merged into the [`dev` branch](https://github.com/cocoonstack/cloud-hypervisor/tree/dev)). virtio-win **0.1.285** now works. No upstream PR exists yet.

**Previous recommendation** (no longer needed): use virtio-win 0.1.240 for Windows VMs on Cloud Hypervisor.

## Windows VM does not respond to ACPI power-button

**Status: FIXED** in our firmware fork.

Cloud Hypervisor uses a GED (Generic Event Device, `ACPI0013`) to deliver power-button notifications on its hardware-reduced ACPI platform. While this mechanism works correctly for Linux guests, Windows guests did not respond to the `vm.power-button` API call — no power-button event appeared in the Windows event log (`Event ID 109`).

**Root cause**: the EFI `ResetSystem` runtime service in [rust-hypervisor-firmware](https://github.com/cloud-hypervisor/rust-hypervisor-firmware) was a no-op. When Windows attempted a graceful shutdown via the UEFI reset path, nothing happened. Tracked in [cloud-hypervisor/rust-hypervisor-firmware#422](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/issues/422) and [cloud-hypervisor/cloud-hypervisor#7929](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7929).

**Fix**: our firmware fork ([cocoonstack/rust-hypervisor-firmware `dev` branch](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev), also [`fix/reset-system` branch](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/fix/reset-system)) implements `ResetSystem` properly. Upstream PR: [cloud-hypervisor/rust-hypervisor-firmware#423](https://github.com/cloud-hypervisor/rust-hypervisor-firmware/pull/423). With this fix, the ACPI power-button works for Windows guests, and `cocoon vm stop` completes in ~13.5 seconds.

**Previous consequence** (no longer applies with our firmware fork): `cocoon vm stop` always timed out on Windows VMs (default 30s), then fell back to `vm.shutdown` → SIGTERM → SIGKILL.

**Previous workaround** (no longer needed with our firmware fork): shut down Windows guests via SSH or WinRM before stopping:

```bash
ssh cocoon@<vm-ip> "shutdown /s /t 0"
cocoon vm stop <vm>
```

Or use `cocoon vm stop --force` to skip the ACPI timeout and immediately kill the process.

The Windows image's `autounattend.xml` includes defensive power-button configuration (`PBUTTONACTION=3`) and shutdown optimization (`WaitToKillServiceTimeout=5000`, `shutdownwithoutlogon=1`) which remain useful for environments not using our firmware fork.

## Installing patched binaries for Windows

See [`os-image/windows/`](os-image/windows/) for download and installation instructions.


## Firecracker snapshot portability

Firecracker snapshots store absolute host paths in the vmstate binary (Rust serde format, not patchable). This means:

- **Same-host clone/restore**: works without restrictions
- **Cross-host export/import**: requires the target host to use **identical `root_dir` and `run_dir`** (default: `/var/lib/cocoon` and `/var/lib/cocoon/run`) and have the **same OCI image pulled**
- **CPU/memory overrides**: not supported on clone/restore — Firecracker cannot change machine config after snapshot/load; `--cpu` and `--memory` flags are rejected if they differ from the snapshot values
- **Drive path redirect**: Cocoon uses a temporary symlink to redirect the source COW path to the clone's COW during `snapshot/load`. This requires a COW flock to serialize with concurrent operations

This is a fundamental Firecracker design limitation. Cloud Hypervisor snapshots do not have this restriction because CH stores device config in a patchable JSON format (`config.json`).

**Upstream fix in progress**: Firecracker [PR #5774](https://github.com/firecracker-microvm/firecracker/pull/5774) adds `drive_overrides` to the `PUT /snapshot/load` API, which would eliminate the symlink redirect and make FC snapshots natively portable. Track this PR for future simplification.

## Firecracker virtio-blk serial numbers

Firecracker does not support virtio-blk serial numbers. Cocoon's OCI init script (`overlay.sh`) uses device paths (`/dev/vdX`) instead of serial names to identify disks when booting under Firecracker. OCI images built from `os-image/ubuntu/overlay.sh` (v0.3+) support both formats automatically. Older images must be rebuilt to work with `--fc`.
