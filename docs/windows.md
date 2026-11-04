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
- **Static IP**: after the rebind, run the printed `netsh interface ipv4 set address ...` inside the guest (SAC serial console via `cocoon vm console`)

For more details, see the [Cloud Hypervisor Windows documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md).
