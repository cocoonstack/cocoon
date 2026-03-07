# Cocoon 改造记录

## 概述

对 cocoon（MicroVM 管理器）做了四处改动，核心目标是**修复 `cocoon vm clone` 后网络不通的 bug**。改动全部在 `dev/chrome-warm-pool-wip` 分支，尚未合入 master。

---

## 1. Clone 后 TAP 重连（核心修复）

**文件**: `hypervisor/cloudhypervisor/clone.go`

### 问题根因

Cloud-Hypervisor (CH) v51.1 的 `vm.restore` API 在反序列化快照时，会把网络设备的 TAP 文件描述符设为 -1（无效值）。CH 源码中有明确注释：

> "FDs in 'RestoredNetConfig' won't be deserialized as they are most likely invalid now. Deserializing them as -1."

结果是：VM 恢复后 virtio-net 后端没有有效的 TAP fd，guest 内部网卡虽然显示存在，但无法收发任何数据包（`tcpdump` 在 host tap0 上完全没有流量）。

### 修复方案：热拔插 + 串口配置

分三步修复：

**第一步：热移除网络设备**（`vm.remove-device`）

- 释放持有无效 fd 的 virtio-net 后端
- CH 从 device tree 中移除该设备

**第二步：热添加网络设备**（`vm.add-net`）

- 用相同的 TAP 名、MAC 地址重新添加
- 关键：必须清空 `id` 字段（`addCfg.ID = ""`），因为 CH 会保留已移除设备的 ID 在 device tree 中，重复 ID 会报错 `"Invalid identifier as it is not unique"`
- CH 在当前 netns 中打开全新的 TAP fd，数据面恢复正常

**第三步：通过串口控制台配置 guest 网卡**

- PCI 热添加改变了 BDF（Bus/Device/Function），guest 内部网卡名从 `ens2` 变成了 `ens6`（新的 PCI slot）
- 新网卡处于 DOWN 状态，没有 IP
- 通过 console.sock（串口 socket）发送登录序列 + IP 配置命令：
  ```bash
  dev=$(ip -o link show | grep -v lo | grep 'state DOWN' | head -1 | awk -F'[ :]+' '{print $2}')
  [ -n "$dev" ] && ip link set "$dev" up && ip addr add <IP>/<prefix> dev "$dev"
  ip route replace default via <gateway>
  ```
- 串口交互时序：`\r\n`(2s) → `root`(2s) → 密码(5s, 等 MOTD) → 配置命令(1s)

### 关键代码

```go
// clone.go — 在 restoreAndResumeClone 中，vm.resume 之后调用
func reconnectNetDevices(ctx context.Context, hc *http.Client, consoleSock string,
    netDevices []chNet, networkConfigs []*types.NetworkConfig, rootPassword string) error {

    for _, nd := range netDevices {
        // 1. 热移除
        removeDevice(ctx, hc, nd.ID)
        // 2. 热添加（清空 ID）
        addCfg := nd
        addCfg.ID = ""
        addNetDevice(ctx, hc, addCfg)
    }
    // 3. 串口配置 guest 网卡
    configureGuestNetworkViaConsole(consoleSock, networkConfigs[0], rootPassword)
    return nil
}
```

### 调用链变更

`Clone()` 函数签名变化：

- `restoreAndResumeClone` 新增两个参数：`netDevices []chNet`、`networkConfigs []*types.NetworkConfig`
- 在调用前先 `parseCHConfig(chConfigPath)` 重新读取 patched 后的 config.json，获取网络设备列表

---

## 2. 热拔插 helper 函数

**文件**: `hypervisor/cloudhypervisor/helper.go`

新增两个函数，封装 CH 的 HTTP API 调用：

```go
func removeDevice(ctx context.Context, hc *http.Client, deviceID string) error
func addNetDevice(ctx context.Context, hc *http.Client, netCfg chNet) error
```

分别调用 `vm.remove-device` 和 `vm.add-net` 端点。

---

## 3. 禁用 balloon（内核兼容性）

**文件**: `hypervisor/cloudhypervisor/api.go`

```go
// 改前
const minBalloonMemory = 256 << 20

// 改后
const minBalloonMemory = 1 << 40 // disabled: kernel 6.12 madvise incompatibility
```

原因：Linux 6.12 内核的 `madvise` 系统调用行为变化导致 CH 的 balloon 设备初始化失败，VM 无法启动。将阈值设为 1TB 等效于完全禁用 balloon。

---

## 4. cloud-init 强制允许 root SSH

**文件**: `metadata/metadata.go`

在 cloud-init user-data 模板中新增 `runcmd` 段：

```yaml
runcmd:
  - sed -i 's/^#*PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - rm -f /etc/ssh/sshd_config.d/60-cloudimg-settings.conf
  - systemctl restart sshd || systemctl restart ssh
```

原因：Ubuntu cloud image 默认禁止 root 密码登录（`60-cloudimg-settings.conf` 覆盖了主配置），导致 `sshpass` 无法连接新建的 VM。

---

## 已知问题

### Console socket 冲突（并发 clone 失败）

连续快速 clone 时，前一个 clone 的 console socket（`/run/cocoon/<vm>/console.sock`）可能还没释放，导致后续 clone 的 `vm.restore` 报错：

```
Error creating console device: Address in use (os error 98)
```

**临时方案**：warm-pool 脚本改为串行 clone（`MAX_CONCURRENT=1`），每次只 clone 一个 VM。

**根本修复**：需要在 `patchCHConfig()` 中确保每个 clone 使用不同的 console socket 路径，或在 `vm.restore` 前确认 socket 文件不存在。这个问题在 `patch.go` 的 `patchCHConfig` 函数中处理——目前已经为每个 clone 生成了独立的 `console.sock` 路径（在 clone 专属的 `runDir` 下），但 CH 可能在某些竞态条件下使用了错误的路径。需要进一步排查。

---

## 测试验证

Clone 修复后验证通过：

```bash
$ sudo cocoon vm clone openclaw-agent-golden --name test-clone --cpu 4 --memory 8G
# 输出 "cloned" 确认成功

$ ping -c 3 10.88.0.29   # clone 的 IP
# 3 packets transmitted, 3 received, 0% packet loss

$ sshpass -p cocoon123 ssh root@10.88.0.29 hostname
# test-clone（SSH 正常）
```

Clone 耗时约 15 秒（含 TAP 重连 + 串口配置），比 `vm run` 的 50 秒快 3 倍以上。
