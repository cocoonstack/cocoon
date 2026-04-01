# Windows VM Image

Build guide for Windows 11 25H2 on Cloud Hypervisor. Unlike the [Cloud Hypervisor manual setup](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md), this guide uses an `autounattend.xml` for fully unattended installation with zero manual interaction.

## Version Requirements

| Component        | Version      | Notes                                                                    |
|------------------|--------------|--------------------------------------------------------------------------|
| Cloud Hypervisor | **v51+**     | Use [CMGS/cloud-hypervisor `dev`][ch-fork] for full Windows support      |
| Firmware         | **patched**  | Use [CMGS/rust-hypervisor-firmware `dev`][fw-fork] for ACPI shutdown     |
| virtio-win       | **0.1.285**  | Latest stable; 0.1.240 also works on upstream CH without patches         |

With our [CH fork][ch-fork] and [firmware fork][fw-fork], all previously known Windows issues are resolved:
- v51 BSOD fixed ([#7849][ch-7849], [PR #7936][ch-7936])
- virtio-win 0.1.285 works ([#7925][ch-7925], ctrl_queue + used_len fix)
- ACPI power-button shutdown works ([firmware#422][fw-422], [firmware PR #423][fw-423])

If using **upstream** (unpatched) Cloud Hypervisor, use v50.2 + virtio-win 0.1.240 + SSH shutdown workaround. See [KNOWN_ISSUES.md](../../KNOWN_ISSUES.md).

[ch-fork]: https://github.com/CMGS/cloud-hypervisor/tree/dev
[fw-fork]: https://github.com/CMGS/rust-hypervisor-firmware/tree/dev
[ch-7849]: https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7849
[ch-7925]: https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7925
[ch-7936]: https://github.com/cloud-hypervisor/cloud-hypervisor/pull/7936
[fw-422]: https://github.com/cloud-hypervisor/rust-hypervisor-firmware/issues/422
[fw-423]: https://github.com/cloud-hypervisor/rust-hypervisor-firmware/pull/423

## Why No CI Build?

Microsoft licensing prohibits public distribution of Windows disk images. GitHub free-tier KVM support is also intermittent and builds take 1-2.5 hours. We distribute the automation code (`autounattend.xml` + this guide) instead of pre-built images.

For internal use with proper licensing, you can automate builds with [Packer + QEMU](https://github.com/norcams/packer-windows) on a self-hosted runner with KVM.

## Prerequisites

- Linux host with KVM
- QEMU (installation phase only -- production runs on Cloud Hypervisor)
- Windows 11 25H2 ISO
- [virtio-win-0.1.285.iso](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.285-1/) (or 0.1.240 for upstream CH)
- OVMF firmware (`OVMF_CODE_4M.secboot.fd`)
- [swtpm](https://github.com/stefanberger/swtpm) (TPM 2.0 emulator -- required by Windows 11)

## Build Steps

### 1. Create disk image

```bash
qemu-img create -f qcow2 windows-11-25h2.qcow2 40G
```

### 2. Prepare OVMF variables

```bash
cp /usr/share/OVMF/OVMF_VARS_4M.fd OVMF_VARS.fd
```

### 3. Start TPM emulator

```bash
mkdir -p /tmp/mytpm
swtpm socket --tpmstate dir=/tmp/mytpm \
  --ctrl type=unixio,path=/tmp/swtpm-sock \
  --tpm2 --log level=20
```

### 4. Embed autounattend.xml

Windows Setup looks for `autounattend.xml` on removable media. Create a floppy image:

```bash
mkfs.fat -C autounattend.img 1440
mcopy -i autounattend.img autounattend.xml ::/autounattend.xml
```

Then add `-drive file=autounattend.img,format=raw,if=floppy` to the QEMU command below.

Alternatively, repack the Windows ISO with `autounattend.xml` at the root.

### 5. Install Windows via QEMU

```bash
qemu-system-x86_64 \
  -machine q35,accel=kvm,smm=on \
  -cpu host,hv_relaxed,hv_spinlocks=0x1fff,hv_vapic,hv_time \
  -m 8G -smp 4 \
  -global driver=cfi.pflash01,property=secure,value=on \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.secboot.fd \
  -drive if=pflash,format=raw,file=OVMF_VARS.fd \
  -cdrom win11_25h2.iso \
  -drive file=virtio-win-0.1.285.iso,index=1,media=cdrom \
  -drive if=none,id=root,file=windows-11-25h2.qcow2,format=qcow2 \
  -device virtio-blk-pci,drive=root,disable-legacy=on \
  -device virtio-net-pci,netdev=mynet0,disable-legacy=on \
  -netdev user,id=mynet0,hostfwd=tcp::2222-:22 \
  -chardev socket,id=chrtpm,path=/tmp/swtpm-sock \
  -tpmdev emulator,id=tpm0,chardev=chrtpm \
  -device tpm-tis,tpmdev=tpm0 \
  -vga std -usb -device usb-tablet \
  -drive file=autounattend.img,format=raw,if=floppy \
  -vnc :0,password=on \
  -monitor unix:/tmp/qemu-monitor.sock,server,nowait \
  -daemonize
```

Connect via VNC to monitor progress. With `autounattend.xml`, the installation is fully automated -- no manual interaction required.

### 6. Post-install verification

After installation completes and the VM reboots into Windows:

```bash
# SSH should be available (forwarded to host port 2222)
ssh -p 2222 cocoon@localhost

# Or connect via VNC and verify in Device Manager:
# - viostor (storage controller)
# - NetKVM (network adapter)
# - Balloon (memory balloon)
# - virtio-win guest tools installed
```

### 7. Post-install verification checklist

After the first boot completes, verify every autounattend.xml configuration persisted correctly.
**Reboot the VM 2-3 times** and re-check after each reboot to confirm persistence.

```powershell
# --- Services ---
Get-Service sshd | Select-Object Status, StartType           # Running, Automatic
Get-Service TermService | Select-Object Status, StartType     # Running, Automatic

# --- RDP ---
(Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Terminal Server').fDenyTSConnections  # 0

# --- SSH ---
Test-NetConnection -ComputerName localhost -Port 22           # TcpTestSucceeded: True

# --- WinRM ---
winrm get winrm/config/service | Select-String "AllowUnencrypted"  # true
winrm get winrm/config/service/auth | Select-String "Basic"        # true
Test-NetConnection -ComputerName localhost -Port 5985               # TcpTestSucceeded: True

# --- SAC / EMS ---
bcdedit /enum | Select-String "ems"                           # Yes for both ems and bootems

# --- Firewall (should be off for dev/test) ---
Get-NetFirewallProfile | Select-Object Name, Enabled          # All: False

# --- Hibernate (should be off) ---
powercfg /a | Select-String "Hibernate"                       # "Hibernation has not been enabled"

# --- ACPI power button action (must be "Shut down" = 3) ---
powercfg /query SCHEME_CURRENT SUB_BUTTONS PBUTTONACTION | Select-String "Current.*Setting"  # 0x00000003

# --- Shutdown optimization ---
(Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control').WaitToKillServiceTimeout      # 5000
(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System').DisableShutdownNamedPipeCheck  # 1
(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System').shutdownwithoutlogon           # 1

# --- Hostname ---
hostname                                                       # COCOON-VM

# --- VirtIO drivers ---
Get-WmiObject Win32_PnPSignedDriver | Where-Object { $_.DeviceName -match 'VirtIO' } | Select-Object DeviceName, DriverVersion
# Expected: VirtIO Balloon Driver, VirtIO SCSI controller, VirtIO Ethernet Adapter (NetKVM)

# --- virtio-win guest tools ---
Get-WmiObject Win32_Product | Where-Object { $_.Name -match 'Virtio-win' } | Select-Object Name, Version
```

If any check fails after reboot, the corresponding autounattend.xml command may not have
executed. Fix manually and re-verify before proceeding.

### 8. Shut down and import to Cocoon

```bash
# Shut down the VM, then import as cloudimg
cocoon image pull <url-or-path-to-windows-11-25h2.qcow2>

# Run with --windows flag
cocoon vm run --windows --name win11 --cpu 2 --memory 4G --storage 40G <image>
```

## autounattend.xml Explained

The included [`autounattend.xml`](autounattend.xml) automates the entire Windows installation across three passes.

### windowsPE pass (installation environment)

- **Language**: en-GB UI, US keyboard (`0409`)
- **VirtIO driver injection**: auto-loads drivers from D: and E: (dual drive letter to handle varying CD-ROM assignment):
  - `viostor` -- virtio storage controller (required for installer to see the disk)
  - `NetKVM` -- virtio network adapter
  - `Balloon` -- virtio memory balloon
  - Both `Win11/amd64/{driver}` (attestation ISO layout) and `{driver}/w11/amd64` (standard ISO layout) paths are searched
- **Disk partitioning**: wipes Disk 0, creates:
  - Partition 1: EFI System (100 MB, FAT32)
  - Partition 2: MSR (16 MB)
  - Partition 3: Windows (remaining space, NTFS, drive C:)
- **Image selection**: ImageIndex=6 (Windows 11 Pro)
- **Product key**: `VK7JG-NPHTM-C97JM-9MPGT-3V66T` (generic install key, not an activation key)
- **EULA**: auto-accepted

### specialize pass (system customization)

- **BypassNRO**: registry hack to skip Win11 mandatory network/Microsoft account requirement during OOBE
- **Hostname**: `COCOON-VM`
- **Timezone**: Pacific Standard Time
- **Locale**: en-US

### oobeSystem pass (first login configuration)

- **User account**: local administrator `cocoon` with auto-logon (password base64-encoded in XML)
- **OOBE**: hides EULA, online account, and wireless setup pages
- **19 FirstLogonCommands** execute in order:

| Order | Action | Command |
|-------|--------|---------|
| 1-2   | **RDP** | Enable Remote Desktop + firewall rule |
| 3-4   | **SSH** | Install OpenSSH Server, auto-start, firewall rule |
| 5     | **ICMP** | Allow ping |
| 6     | **Firewall** | Disable all profiles (dev/test environment) |
| 7     | **Hibernate** | Disable (`powercfg /h off`) |
| 8-10  | **SAC serial console** | `bcdedit /ems on`, `/bootems on`, emsport:1 emsbaudrate:115200 |
| 11    | **Terminal Service** | Set to auto-start |
| 12    | **EMS-SAC tools** | Install Windows Desktop EMS-SAC capability |
| 13    | **Network profile** | Set to Private (required for WinRM) |
| 14-17 | **WinRM** | Enable PS Remoting, allow unencrypted + Basic auth, firewall on port 5985 |
| 18    | **Hostname** | Force rename (specialize `ComputerName` unreliable on 25H2) |
| 19    | **virtio-win guest tools** | Silent install `virtio-win-gt-x64.msi` from CD-ROM (D: or E:) -- installs complete driver suite + balloon service |
| 20-22 | **ACPI power button = shut down** | Set power button action to "Shut down" for AC/DC power schemes -- works with our [firmware fork][fw-fork]; defensive config for upstream firmware |
| 23-24 | **Shutdown optimization** | Reduce `WaitToKillServiceTimeout` to 5s, disable shutdown named pipe check -- speeds up `shutdown /s /t 0` via SSH/WinRM |
| 25    | **Shutdown without logon** | Allow remote shutdown when no user is logged in (required for SSH/WinRM `shutdown /s /t 0`) |

## Post-Clone Networking

- **DHCP networks**: no action needed, Windows DHCP client auto-configures on new NIC
- **Static IP**: configure via SAC serial console (`cocoon vm console`):
  ```
  cmd
  ch -si 1
  netsh interface ip set address "Ethernet" static <IP> <MASK> <GW>
  ```
  See the [Cloud Hypervisor Windows documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md) for details.
