# Windows Guests

Running Windows VMs with the cocoonstack Cloud Hypervisor and firmware forks.

## Overview

Cocoon supports Windows guests via the `--windows` flag:

```bash
cocoon vm run --windows --name win11 --cpu 2 --memory 4G --storage 15G <cloudimg-url>
```

The `--windows` flag:
- Forces UEFI firmware boot (cloudimg path)
- Enables Hyper-V enlightenments (`kvm_hyperv=on`)
- Skips cloud-init cidata disk generation (Windows does not use cloud-init)

### Requirements

- Cloud Hypervisor **v51+** with our [CH fork](https://github.com/cocoonstack/cloud-hypervisor/tree/dev) (includes DISCARD fix, virtio-net ctrl_queue fix, and upstream patches — see [known issues](known-issues.md))
- UEFI firmware from our [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) (includes ResetSystem fix for ACPI power-button — see [known issues](known-issues.md))
- virtio-win **0.1.285** drivers pre-installed in the image (0.1.240 also works for any version of Cloud Hypervisor; newer versions are supported with our CH fork)

### Image

Pre-built images and build automation are maintained in [cocoonstack/windows](https://github.com/cocoonstack/windows).

The Windows image is published as an **OCI artifact** (split qcow2 parts pushed via ORAS), not a runnable OCI container image — use `oras pull` (not `cocoon image pull` or `docker pull`).

```bash
# 1. Pull split parts via oras (https://oras.land)
oras pull ghcr.io/cocoonstack/windows/win11:25h2

# 2. Reassemble and verify
cat windows-11-25h2.qcow2.*.qcow2.part > windows-11-25h2.qcow2
sha256sum -c SHA256SUMS

# 3. Import into Cocoon
cocoon image import win11-25h2 windows-11-25h2.qcow2
```


### Post-Clone Networking

- **DHCP networks**: no action needed, Windows DHCP client auto-configures
- **Static IP**: configure via SAC serial console (`cocoon vm console`)

For more details, see the [Cloud Hypervisor Windows documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md).
