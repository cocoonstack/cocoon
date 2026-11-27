# Windows Guests

Running Windows VMs with the cocoonstack Cloud Hypervisor and firmware forks.

## Overview

Cocoon supports Windows guests via the `--windows` flag:

```bash
cocoon image pull <cloudimg-url>
cocoon vm run --windows --name win11 --cpu 2 --memory 4G --storage 15G <image-name>
```

The `--windows` flag:
- Requires a cloudimg (UEFI firmware) image; OCI direct-boot images are rejected
- Enables Hyper-V enlightenments (`kvm_hyperv=on`)
- Skips cloud-init cidata disk generation (Windows does not use cloud-init)
- Omits the virtio-balloon device (the virtio-win driver hangs on deflation)

### Requirements

- Cloud Hypervisor **v54 or newer**: `cocoon-check --upgrade` installs the [cocoonstack fork](https://github.com/cocoonstack/cloud-hypervisor/tree/dev) `dev` release build. The virtio-blk DISCARD fix and the virtio-net ctrl_queue tolerance and `used_len` fix are upstream since v52; the fork adds diff snapshots — see [known issues](known-issues.md)
- UEFI firmware from our [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) `dev` build, what `cocoon-check --upgrade` installs on x86_64 (EFI ResetSystem for the ACPI power-button and the IA32_FEATURE_CONTROL/VMXON lock — see [known issues](known-issues.md)); upstream 0.5.0 boots Windows but `cocoon vm stop` falls back to the 30s timeout
- virtio-win **0.1.285** drivers pre-installed in the image (0.1.240 also works; newer versions need the ctrl_queue tolerance, i.e. Cloud Hypervisor v52 or newer)

### Image

Pre-built images are published as split OCI artifacts and imported with `oras pull` + `cocoon image import`; see [OS images](os-image.md#windows) for the pull steps and [cocoonstack/windows](https://github.com/cocoonstack/windows) for build automation.

### Post-Clone Networking

- **DHCP networks**: the clone comes up on hot-swapped NICs with new MACs; if a NIC is left without IPv4, run the `Get-PnpDevice` rebind that `cocoon vm clone` prints
- **Static IP**: after the rebind, run the printed `netsh interface ipv4 set address ...` inside the guest (SAC serial console via `cocoon vm console`). Windows keeps a static IPv4 setting per PCI slot, so a clone NIC that lands in the source's slot re-applies the source's address and collides with any running VM that holds it; prefer DHCP for clone sources, or reset the adapter's settings in the clone

### Clock After Clone and Restore

`cocoon vm clone` and `cocoon vm restore` resume a Windows guest from the snapshot's clock (`clock_restore_mode=Preserve` on Cloud Hypervisor's restore). The guest's uptime continues from the snapshot, and its date and time are behind by the snapshot's age until something sets them in the guest, for example `Set-Date` over `cocoon vm exec`; `vm clone` prints the command. A hibernated Windows VM wakes the same way, behind by the time it slept.

Cloud Hypervisor's default advances the clock by the snapshot's age, which moves a Windows guest's uptime forward by as much. A jump of 2^31 to 2^32 ms (24.9 to 49.7 days) can leave the guest unable to retransmit TCP ([cloud-hypervisor#9](https://github.com/cocoonstack/cloud-hypervisor/issues/9)), and a snapshot taken from such a guest hands the fault to its clones. Linux guests keep the default, and so does a `vm migrate` receive, where the gap is the seconds the handoff took.

This needs a Cloud Hypervisor that knows `clock_restore_mode`: upstream from 2026-11-23, the cocoonstack `dev` build from v56.0.0-dev-5fd961f7. An older one ignores the field and advances the clock.

For more details, see the [Cloud Hypervisor Windows documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md).
