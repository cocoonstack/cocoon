# Cocoon Daemon: Design & Implementation Plan

## Goal

Add a gRPC daemon to Cocoon so that external services can manage VMs via remote calls.
The daemon is a **new entry point** alongside the existing CLI. Both share the same
business logic layer and can run concurrently on the same host without conflict.

---

## Table of Contents

1. [Current Architecture](#1-current-architecture)
2. [Target Architecture](#2-target-architecture)
3. [Test Coverage Assessment](#3-test-coverage-assessment)
4. [Service Layer Design](#4-service-layer-design)
5. [Daemon Layer Design](#5-daemon-layer-design)
6. [Implementation Plan (Phases)](#6-implementation-plan-phases)
7. [File-by-File Change Spec](#7-file-by-file-change-spec)
8. [Testing Strategy](#8-testing-strategy)
9. [Migration Safety Rules](#9-migration-safety-rules)
10. [Code Style Rules](#10-code-style-rules)

---

## 1. Current Architecture

```
cobra CLI  ──→  cmd/vm/handler.go       ──→  hypervisor.Hypervisor
                cmd/images/handler.go    ──→  images.Images
                cmd/snapshot/handler.go  ──→  snapshot.Snapshot
                cmd/others/handler.go    ──→  network.Network, gc.Orchestrator
```

Each handler method has this pattern:

```go
func (h Handler) Run(cmd *cobra.Command, args []string) error {
    // 1. Parse cobra flags → typed Go values
    ctx, conf, err := h.Init(cmd)
    vmCfg, err := cmdcore.VMConfigFromFlags(cmd, args[0])
    nics, _ := cmd.Flags().GetInt("nics")

    // 2. Initialize backends
    backends, hyper, err := cmdcore.InitBackends(ctx, conf)

    // 3. Business logic (the real work)
    storageConfigs, bootCfg, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
    vmID, _ := utils.GenerateID()
    _, networkConfigs, err := initNetwork(ctx, conf, vmID, nics, vmCfg)
    vm, err := hyper.Create(ctx, vmID, vmCfg, storageConfigs, networkConfigs, bootCfg)
    hyper.Start(ctx, []string{vm.ID})

    // 4. Output formatting (print to terminal)
    logger.Infof(ctx, "VM created: %s", vm.ID)
}
```

**Problem**: Steps 1-4 are tangled. A daemon cannot reuse step 3 without dragging
in cobra (step 1) and terminal output (step 4).

### Key source files

| File | Role |
|------|------|
| `cmd/root.go` | Cobra root command, config init, signal handling |
| `cmd/core/helpers.go` | `BaseHandler`, `Init*()` factory functions, `VMConfigFromFlags()`, `ResolveImage()` |
| `cmd/vm/handler.go` | VM handlers: Create, Run, Clone, Start, Stop, List, Inspect, Console, RM, Restore, Debug |
| `cmd/images/handler.go` | Image handlers: Pull, Import, List, RM, Inspect |
| `cmd/snapshot/handler.go` | Snapshot handlers: Save, List, Inspect, RM |
| `cmd/others/handler.go` | GC, Version |

### Existing interfaces (unchanged)

These backend interfaces are **not modified** by this design:

- `hypervisor.Hypervisor` — Create/Start/Stop/Delete/Snapshot/Clone/Restore/Inspect/List/Console
- `hypervisor.Direct` — DirectClone/DirectRestore (optional fast path)
- `images.Images` — Pull/Import/Config/List/Delete/Inspect
- `network.Network` — Config/Delete/Verify/Inspect/List
- `snapshot.Snapshot` — Create/List/Inspect/Delete/Restore
- `snapshot.Direct` — DataDir (optional fast path)
- `storage.Store[T]` — With/Update/TryLock/Unlock
- `lock.Locker` — Lock/Unlock/TryLock

---

## 2. Target Architecture

```
cobra CLI  ──→  cmd/*/handler.go  ──→  flag parsing  ──→  service.*  ──→  backends
                                                              ↑
gRPC server  ──→  daemon/server.go  ──→  protobuf decode ────┘
```

Three layers:

| Layer | Package | Responsibility | Depends on cobra? |
|-------|---------|---------------|-------------------|
| **Entry points** | `cmd/*/handler.go`, `daemon/` | Parse input (flags or protobuf), format output | CLI: yes, Daemon: no |
| **Service** | `service/` | All business logic, backend orchestration | No |
| **Backends** | `hypervisor/`, `images/`, `network/`, `snapshot/` | Low-level operations | No |

### Concurrency model

The daemon does **not** claim exclusive ownership. Locking stays on flock (file-level),
so CLI and daemon can run concurrently just like multiple CLI processes do today.

```
CLI direct  ──flock──→  JSON DB  ←──flock──  daemon
```

---

## 3. Test Coverage Assessment

### Current state (as of commit ea0e9b2)

| Layer | Test files | Coverage | Notes |
|-------|-----------|----------|-------|
| `utils/` | 14 files | Excellent | Atomic, batch, file, http, process, tar, sparse, ... |
| `snapshot/localfile/` | 1 file | Excellent | All CRUD + restore + data dir |
| `metadata/` | 1 file | Good | Template generation, FAT12 |
| `config/` | 1 file | Basic | Validation only |
| `hypervisor/cloudhypervisor/` | 1 file | Partial | Clone config patching only |
| `cmd/core/` | 1 file | Minimal | `sanitizeVMName` only |
| `cmd/vm/` | 0 files | **None** | No handler tests at all |
| `cmd/images/` | 0 files | **None** | |
| `cmd/snapshot/` | 0 files | **None** | |
| `network/` | 0 files | **None** | |
| `images/` (backends) | 0 files | **None** | |

### Key gap

The business logic we are extracting (from `cmd/*/handler.go`) has **zero test coverage**.
We cannot refactor safely without adding tests first.

### Why we test the service layer, not the handlers

Testing handlers directly is hard — they take `*cobra.Command` and write to stdout.
The refactoring *itself* creates the testable surface: once logic moves to `service/`,
we can test it with mock backends. The handlers become thin (flag parsing + output)
and less likely to break.

The strategy: **add tests on the new service layer as part of the extraction**, then
verify the CLI still behaves correctly with a small set of end-to-end smoke tests.

---

## 4. Service Layer Design

### 4.1 Package structure

```
service/
    service.go      # Service struct (holds all backends), constructor
    vm.go           # VM operations
    image.go        # Image operations
    snapshot.go     # Snapshot operations
    gc.go           # GC operation
    params.go       # All parameter structs (no cobra dependency)
```

### 4.2 Service struct

```go
// service/service.go
package service

import (
    "context"

    "github.com/projecteru2/cocoon/config"
    "github.com/projecteru2/cocoon/hypervisor"
    imagebackend "github.com/projecteru2/cocoon/images"
    "github.com/projecteru2/cocoon/images/cloudimg"
    "github.com/projecteru2/cocoon/images/oci"
    "github.com/projecteru2/cocoon/network"
    "github.com/projecteru2/cocoon/snapshot"
)

// Service provides all Cocoon operations, independent of transport (CLI/gRPC).
type Service struct {
    conf         *config.Config
    hypervisor   hypervisor.Hypervisor
    backends     []imagebackend.Images
    ociStore     *oci.OCI           // concrete type needed for Pull
    cloudimgStore *cloudimg.CloudImg // concrete type needed for Pull
    network      network.Network
    snapshot     snapshot.Snapshot
}

// New creates a Service by initializing all backends.
// This is the single place where backends are wired together.
func New(ctx context.Context, conf *config.Config) (*Service, error) {
    ociStore, err := oci.New(ctx, conf)
    if err != nil {
        return nil, fmt.Errorf("init oci backend: %w", err)
    }

    cloudimgStore, err := cloudimg.New(ctx, conf)
    if err != nil {
        return nil, fmt.Errorf("init cloudimg backend: %w", err)
    }

    hyper, err := cloudhypervisor.New(conf)
    if err != nil {
        return nil, fmt.Errorf("init hypervisor: %w", err)
    }

    netProvider, err := cni.New(conf)
    if err != nil {
        return nil, fmt.Errorf("init network: %w", err)
    }

    snapBackend, err := localfile.New(conf)
    if err != nil {
        return nil, fmt.Errorf("init snapshot: %w", err)
    }

    return &Service{
        conf:          conf,
        hypervisor:    hyper,
        backends:      []imagebackend.Images{ociStore, cloudimgStore},
        ociStore:      ociStore,
        cloudimgStore: cloudimgStore,
        network:       netProvider,
        snapshot:      snapBackend,
    }, nil
}

// Config returns the active config (for callers that need path info, etc.).
func (s *Service) Config() *config.Config { return s.conf }
```

**Why hold backends as struct fields instead of creating per-call?**

Currently, handlers create backends on every invocation (`InitHypervisor(conf)` inside
each method). This works for CLI where each invocation is a short-lived process. For
the daemon, we want to initialize once and reuse. The backends themselves are safe to
reuse — they hold a `storage.Store` (file-backed with flock) that locks per-operation.

**Alternative for testing**: A constructor that accepts interfaces directly:

```go
// NewWithBackends is used by tests to inject mock backends.
func NewWithBackends(
    conf *config.Config,
    hyper hypervisor.Hypervisor,
    backends []imagebackend.Images,
    net network.Network,
    snap snapshot.Snapshot,
) *Service {
    return &Service{
        conf:       conf,
        hypervisor: hyper,
        backends:   backends,
        network:    net,
        snapshot:   snap,
    }
}
```

### 4.3 Parameter structs

```go
// service/params.go
package service

// VMCreateParams contains all inputs for creating a VM.
type VMCreateParams struct {
    Image   string // image reference (OCI tag or cloudimg URL)
    Name    string // VM name (optional, auto-generated if empty)
    CPU     int
    Memory  int64  // bytes (already parsed from "1G" etc.)
    Storage int64  // bytes
    NICs    int
    Network string // CNI conflist name
}

// VMCloneParams contains all inputs for cloning a VM from a snapshot.
type VMCloneParams struct {
    SnapshotRef string
    Name        string
    CPU         int    // 0 = inherit from snapshot
    Memory      int64  // 0 = inherit
    Storage     int64  // 0 = inherit
    NICs        int    // 0 = inherit
    Network     string // "" = inherit
}

// VMRestoreParams contains inputs for restoring a VM to a snapshot.
type VMRestoreParams struct {
    VMRef       string
    SnapshotRef string
    CPU         int   // 0 = keep current
    Memory      int64 // 0 = keep current
    Storage     int64 // 0 = keep current
}

// VMStopParams contains inputs for stopping VM(s).
type VMStopParams struct {
    Refs  []string
    Force bool // unused currently, reserved for future use
}

// VMRMParams contains inputs for deleting VM(s).
type VMRMParams struct {
    Refs  []string
    Force bool // stop running VMs before deletion
}

// SnapshotSaveParams contains inputs for saving a snapshot.
type SnapshotSaveParams struct {
    VMRef       string
    Name        string
    Description string
}

// ImagePullParams contains inputs for pulling an image.
type ImagePullParams struct {
    Refs []string // image references or URLs
}

// ImageImportParams contains inputs for importing local files as an image.
type ImageImportParams struct {
    Name  string
    Files []string
}

// DebugParams contains inputs for the debug command.
type DebugParams struct {
    VMCreateParams
    MaxCPU  int
    Balloon int
    COWPath string
    CHBin   string
}
```

### 4.4 VM service methods

Below is the **exact** extraction from each handler method. Each service method
contains the business logic that was previously in the handler, minus cobra flag
parsing and terminal output.

```go
// service/vm.go

// ----- Create -----
// Source: cmd/vm/handler.go Handler.createVM() lines 508-546
// + Handler.Create() lines 33-41
//
// Extracts:
//   - Image resolution (ResolveImage)
//   - ID generation
//   - Network setup
//   - hyper.Create call
//   - Network rollback on failure
//
// Returns: *types.VM so CLI can print, daemon can serialize to protobuf.

func (s *Service) CreateVM(ctx context.Context, p VMCreateParams) (*types.VM, error)

// ----- Run -----
// Source: cmd/vm/handler.go Handler.Run() lines 44-60
//
// Calls CreateVM + Start atomically.
// Returns: *types.VM (in running state)

func (s *Service) RunVM(ctx context.Context, p VMCreateParams) (*types.VM, error)

// ----- Clone -----
// Source: cmd/vm/handler.go Handler.Clone() lines 62-115
//        Handler.cloneDirect() lines 117-139
//        Handler.prepareClone() lines 141-169
//
// NOTE: prepareClone currently takes *cobra.Command for flag extraction.
// After extraction, it will take VMCloneParams directly.
//
// Logic to extract:
//   1. Build VMConfig from VMCloneParams + SnapshotConfig (replaces CloneVMConfigFromFlags)
//   2. Generate VM ID, default name
//   3. Validate NICs >= snapshot NICs
//   4. Setup network
//   5. Try DirectClone, fallback to stream Clone
//   6. Rollback network on failure
//
// Returns: *types.VM, []*types.NetworkConfig (for post-clone hints)

func (s *Service) CloneVM(ctx context.Context, p VMCloneParams) (*types.VM, []*types.NetworkConfig, error)

// ----- Start -----
// Source: cmd/vm/handler.go Handler.Start() lines 198-214
//        Handler.recoverNetwork() lines 219-234
//
// Includes network recovery (pre-start check for missing netns).
// Returns: []string (started VM IDs)

func (s *Service) StartVM(ctx context.Context, refs []string) ([]string, error)

// ----- Stop -----
// Source: cmd/vm/handler.go Handler.Stop() lines 236-242
// Returns: []string (stopped VM IDs)

func (s *Service) StopVM(ctx context.Context, refs []string) ([]string, error)

// ----- List -----
// Source: cmd/vm/handler.go Handler.List() lines 244-272
// Returns: []*types.VM (sorted by CreatedAt)
// NOTE: ReconcileState is NOT applied here — that's a display concern.
//       The caller (CLI or daemon) applies it when formatting output.

func (s *Service) ListVM(ctx context.Context) ([]*types.VM, error)

// ----- Inspect -----
// Source: cmd/vm/handler.go Handler.Inspect() lines 274-286
// Returns: *types.VM

func (s *Service) InspectVM(ctx context.Context, ref string) (*types.VM, error)

// ----- Console -----
// Source: cmd/vm/handler.go Handler.Console() lines 288-340
//
// The service layer only returns the raw connection (io.ReadWriteCloser).
// Terminal setup, escape handling, and resize propagation stay in the CLI
// handler — they are terminal concerns, not business logic.
//
// For daemon: the gRPC bidirectional stream wraps this connection.

func (s *Service) ConsoleVM(ctx context.Context, ref string) (io.ReadWriteCloser, error)

// ----- RM -----
// Source: cmd/vm/handler.go Handler.RM() lines 345-380
//
// Includes: hyper.Delete + network cleanup for deleted VMs
// Returns: []string (deleted VM IDs)

func (s *Service) RemoveVM(ctx context.Context, p VMRMParams) ([]string, error)

// ----- Restore -----
// Source: cmd/vm/handler.go Handler.Restore() lines 382-453
//        Handler.restoreDirect() lines 171-196
//
// Validates: snapshot belongs to VM, NIC count matches,
//            resources >= snapshot minimums.
// Returns: *types.VM

func (s *Service) RestoreVM(ctx context.Context, p VMRestoreParams) (*types.VM, error)

// ----- Debug -----
// Source: cmd/vm/handler.go Handler.Debug() lines 455-492
//
// Returns structured data instead of printing to stdout.
// The CLI handler formats it into the cloud-hypervisor command string.

type DebugInfo struct {
    StorageConfigs []*types.StorageConfig
    BootConfig     *types.BootConfig
    VMConfig       *types.VMConfig
}

func (s *Service) DebugVM(ctx context.Context, p DebugParams) (*DebugInfo, error)
```

### 4.5 Image service methods

```go
// service/image.go

// Pull dispatches to OCI or cloudimg based on ref format.
// Progress events are sent via the callback.
// Source: cmd/images/handler.go Handler.Pull() lines 29-51
//         Handler.pullOCI() lines 213-231
//         Handler.pullCloudimg() lines 233-262
func (s *Service) PullImage(ctx context.Context, ref string, tracker progress.Tracker) error

// Import auto-detects file type and imports.
// Source: cmd/images/handler.go Handler.Import() lines 53-73
//         Handler.importOCI() lines 75-97
//         Handler.importCloudimg() lines 99-121
func (s *Service) ImportImage(ctx context.Context, name string, files []string, tracker progress.Tracker) error

// List returns all images across all backends.
// Source: cmd/images/handler.go Handler.List() lines 123-159
func (s *Service) ListImages(ctx context.Context) ([]*types.Image, error)

// RM deletes images by reference across all backends.
// Source: cmd/images/handler.go Handler.RM() lines 161-187
func (s *Service) RemoveImages(ctx context.Context, refs []string) ([]string, error)

// Inspect finds an image by reference.
// Source: cmd/images/handler.go Handler.Inspect() lines 189-211
func (s *Service) InspectImage(ctx context.Context, ref string) (*types.Image, error)
```

### 4.6 Snapshot service methods

```go
// service/snapshot.go

// Save snapshots a running VM.
// Source: cmd/snapshot/handler.go Handler.Save() lines 24-80
func (s *Service) SaveSnapshot(ctx context.Context, p SnapshotSaveParams) (string, error)

// List returns all snapshots, optionally filtered by VM.
// Source: cmd/snapshot/handler.go Handler.List() lines 82-143
func (s *Service) ListSnapshots(ctx context.Context, vmRef string) ([]*types.Snapshot, error)

// Inspect returns a snapshot by reference.
// Source: cmd/snapshot/handler.go Handler.Inspect() lines 145-160
func (s *Service) InspectSnapshot(ctx context.Context, ref string) (*types.Snapshot, error)

// RM deletes snapshots.
// Source: cmd/snapshot/handler.go Handler.RM() lines 162-184
func (s *Service) RemoveSnapshots(ctx context.Context, refs []string) ([]string, error)
```

### 4.7 GC service method

```go
// service/gc.go

// RunGC performs cross-module garbage collection.
// Source: cmd/others/handler.go Handler.GC() lines 18-48
func (s *Service) RunGC(ctx context.Context) error
```

### 4.8 What stays in the handler (CLI layer)

After extraction, each handler method becomes a thin wrapper:

```go
// Example: cmd/vm/handler.go after refactoring
func (h Handler) Run(cmd *cobra.Command, args []string) error {
    ctx, svc, err := h.initService(cmd)  // new helper
    if err != nil {
        return err
    }

    // Step 1: Parse flags → params (CLI-specific)
    params, err := vmCreateParamsFromFlags(cmd, args[0])
    if err != nil {
        return err
    }

    // Step 2: Call service (shared with daemon)
    vm, err := svc.RunVM(ctx, params)
    if err != nil {
        return err
    }

    // Step 3: Format output (CLI-specific)
    logger := log.WithFunc("cmd.run")
    logger.Infof(ctx, "VM created: %s (name: %s)", vm.ID, vm.Config.Name)
    logger.Infof(ctx, "started: %s", vm.ID)
    return nil
}
```

The handler retains **only**:
- Cobra flag parsing → `VMCreateParams`
- Calling `svc.Method(params)`
- Printing results to terminal

### 4.9 Config flag parsing functions

These functions replace the cobra-coupled `VMConfigFromFlags` etc. They stay in
`cmd/core/helpers.go` because they are CLI-specific:

```go
// cmd/core/helpers.go (updated)

// vmCreateParamsFromFlags builds VMCreateParams from cobra flags.
// This replaces VMConfigFromFlags for the service layer.
func VMCreateParamsFromFlags(cmd *cobra.Command, image string) (service.VMCreateParams, error)

// vmCloneParamsFromFlags builds VMCloneParams from cobra flags.
func VMCloneParamsFromFlags(cmd *cobra.Command, snapshotRef string) (service.VMCloneParams, error)

// vmRestoreParamsFromFlags builds VMRestoreParams from cobra flags.
func VMRestoreParamsFromFlags(cmd *cobra.Command, vmRef, snapRef string) (service.VMRestoreParams, error)
```

The existing `VMConfigFromFlags`, `CloneVMConfigFromFlags`, `RestoreVMConfigFromFlags`
are **removed** after migration. The new functions produce `service.*Params` structs
instead of `*types.VMConfig`. The service layer internally converts params to VMConfig.

---

## 5. Daemon Layer Design

### 5.1 gRPC service definition

```
proto/
    cocoon/v1/
        vm.proto
        image.proto
        snapshot.proto
        gc.proto
```

**Why gRPC?**
- `Console` needs bidirectional streaming → gRPC `stream` is native
- `Pull` needs server-side progress streaming → gRPC server stream
- Typed contracts via protobuf → auto-generated client code
- Unix domain socket support is built-in

### 5.2 Daemon package

```
daemon/
    daemon.go       # Daemon struct: holds *service.Service, starts gRPC server
    vm.go           # cocoon.v1.VMService server implementation
    image.go        # cocoon.v1.ImageService server implementation
    snapshot.go     # cocoon.v1.SnapshotService server implementation
    gc.go           # cocoon.v1.GCService server implementation

cmd/daemon/
    commands.go     # cocoon daemon {start|stop|status}
    handler.go
```

### 5.3 Daemon lifecycle

```go
// daemon/daemon.go

type Daemon struct {
    svc    *service.Service
    server *grpc.Server
    socket string // e.g. /var/run/cocoon/cocoon.sock
}

func (d *Daemon) Start(ctx context.Context) error {
    // 1. Write PID file
    // 2. Listen on unix socket
    // 3. Register gRPC services
    // 4. Serve (blocks until ctx cancelled or Stop called)
    // 5. Cleanup: remove socket, PID file
}

func (d *Daemon) Stop() {
    d.server.GracefulStop()
}
```

### 5.4 CLI dual-mode detection

```go
// cmd/root.go additions

// New persistent flag:
cmd.PersistentFlags().String("host", "", "daemon address (unix:///path or tcp://host:port)")

// In cmd/core/helpers.go, new method:
func (h BaseHandler) InitService(cmd *cobra.Command) (*service.Service, error) {
    // Always create a local service — even if daemon exists,
    // CLI uses local service (they share flock, no conflict).
    conf, err := h.Conf()
    ctx := CommandContext(cmd)
    return service.New(ctx, conf)
}
```

**Design decision**: CLI always runs locally. The `--host` flag is for a *separate*
`cocoon` client binary or SDK consumers. The CLI itself does not proxy through the
daemon — it calls the service layer directly, same as today. This keeps the CLI
behavior 100% identical and avoids the complexity of a client-mode CLI.

The daemon is purely for **external consumers** (other services, SDKs, web UIs).

---

## 6. Implementation Plan (Phases)

### Phase 0: Pre-flight checks
- [ ] Verify `make test` passes on current master
- [ ] Verify `make lint` passes
- [ ] Record baseline: `go test -cover ./...` output

### Phase 1: Add service-layer tests (before any code moves)

**Goal**: Create the test infrastructure and write tests against the *interfaces*
that the service layer will expose. These tests use mock backends and define the
expected behavior contract.

**Why first?** We need a safety net before moving code. Since the handlers have
zero tests, and the handlers are hard to test (cobra coupling), we write tests
for the service layer API *first*, with mock backends. When we then extract the
logic in Phase 2, the tests immediately validate correctness.

**Steps**:

1. Create `service/params.go` with all parameter structs (data-only, no logic)
2. Create interface file `service/service.go` with the `Service` struct definition
   and the `NewWithBackends` constructor (for test injection)
3. Create mock backends in `service/testutil_test.go`:
   - `mockHypervisor` implementing `hypervisor.Hypervisor`
   - `mockImages` implementing `images.Images`
   - `mockNetwork` implementing `network.Network`
   - `mockSnapshot` implementing `snapshot.Snapshot`
4. Write test files:
   - `service/vm_test.go` — test each VM operation
   - `service/image_test.go` — test each image operation
   - `service/snapshot_test.go` — test each snapshot operation
   - `service/gc_test.go` — test GC orchestration

**Test cases to cover for each method** (table-driven):

```
CreateVM:
  - success: mock returns VM → verify returned VM matches
  - image not found: ResolveImage fails → error propagated
  - network failure: network.Config fails → verify rollback called
  - create failure: hyper.Create fails → verify network rollback

RunVM:
  - success: create + start succeed
  - create fails: start not called
  - start fails: error propagated (VM exists in created state)

CloneVM:
  - success with direct path
  - success with stream path (mock has no Direct interface)
  - snapshot not found: error
  - NICs below minimum: error
  - CPU below minimum: error
  - network failure: verify rollback

StartVM:
  - success: returns started IDs
  - with network recovery: verify recovery attempted for stale netns
  - partial failure: some VMs start, some fail

StopVM:
  - success: returns stopped IDs

ListVM:
  - returns sorted list
  - empty list

InspectVM:
  - found: returns VM
  - not found: error

ConsoleVM:
  - returns io.ReadWriteCloser from hypervisor

RemoveVM:
  - success: returns deleted IDs + network cleaned up
  - force stop: --force triggers stop before delete
  - partial: some deleted, some fail → still cleans up successful ones

RestoreVM:
  - success with direct path
  - success with stream path
  - snapshot not owned by VM: error
  - NIC count mismatch: error
  - resource below minimum: error

SaveSnapshot:
  - success: returns snapshot ID
  - duplicate name: error
  - VM not found: error

PullImage:
  - OCI ref → ociStore.Pull called
  - URL ref → cloudimgStore.Pull called

ListImages:
  - aggregates from all backends

RemoveImages:
  - delegates to all backends, aggregates results

RunGC:
  - all modules registered and Run called
```

### Phase 2: Extract service layer

**Goal**: Move business logic from handlers to `service/` package. Pure mechanical
refactoring — no behavior changes.

**Steps**:

1. Create `service/vm.go` — implement each method by copying logic from handler:
   - `CreateVM`: from `Handler.createVM()` (lines 508-546 of cmd/vm/handler.go)
   - `RunVM`: from `Handler.Run()` (lines 44-60)
   - `CloneVM`: from `Handler.Clone()` + `cloneDirect()` + `prepareClone()`
   - `StartVM`: from `Handler.Start()` + `recoverNetwork()`
   - `StopVM`: from `Handler.Stop()`
   - `ListVM`: from `Handler.List()` (without output formatting)
   - `InspectVM`: from `Handler.Inspect()` (without JSON output)
   - `ConsoleVM`: from `Handler.Console()` (return conn only)
   - `RemoveVM`: from `Handler.RM()`
   - `RestoreVM`: from `Handler.Restore()` + `restoreDirect()`
   - `DebugVM`: from `Handler.Debug()` (return data only)
2. Create `service/image.go` — from `cmd/images/handler.go`
3. Create `service/snapshot.go` — from `cmd/snapshot/handler.go`
4. Create `service/gc.go` — from `cmd/others/handler.go`
5. Update `cmd/vm/handler.go`: replace logic with service calls
6. Update `cmd/images/handler.go`: same
7. Update `cmd/snapshot/handler.go`: same
8. Update `cmd/others/handler.go`: same
9. Update `cmd/core/helpers.go`:
   - Add `VMCreateParamsFromFlags`, `VMCloneParamsFromFlags`, `VMRestoreParamsFromFlags`
   - Keep existing `Init*()` factory functions (still used by `service.New()`)
   - Remove `VMConfigFromFlags`, `CloneVMConfigFromFlags`, `RestoreVMConfigFromFlags`
     after handlers are migrated (or keep as deprecated if external consumers exist)
10. Run `make test` — all Phase 1 tests + existing tests must pass
11. Run `make lint`

**Extraction rules** (see Section 9 for full list):
- Never change backend interface signatures
- Never change types/ structs
- Service methods return data, never print to stdout
- Service methods take `context.Context` as first arg
- Error wrapping format stays consistent with current code
- `initNetwork` helper moves to service package (private)
- `rollbackNetwork` helper moves to service package (private)

### Phase 3: CLI integration smoke tests

**Goal**: Verify the refactored CLI still works end-to-end.

**Steps**:

1. Create `test/integration/` directory
2. Write shell-based or Go-based integration tests:
   - `test/integration/cli_test.go` (build tag: `//go:build integration`)
   - Tests run `cocoon` binary and verify exit codes + output patterns
3. Test cases (require KVM, so only run in CI with `--tags integration`):
   - `cocoon version` → exit 0, contains version string
   - `cocoon image list` → exit 0 (may be empty)
   - `cocoon vm list` → exit 0
   - `cocoon gc` → exit 0
   - (If test image available) `cocoon vm create` + `inspect` + `rm`
4. These tests are **additive** — they don't replace unit tests

### Phase 4: Add daemon

**Goal**: Implement the gRPC daemon as a new entry point.

**Steps**:

1. Define protobuf schemas in `proto/cocoon/v1/`
2. Generate Go code: `buf generate` or `protoc`
3. Implement `daemon/` package:
   - Each gRPC service method calls `service.Service` methods
   - Console: wrap bidirectional gRPC stream as `io.ReadWriteCloser`
   - Pull/Import: wrap `progress.Tracker` to send gRPC server-stream events
4. Implement `cmd/daemon/` commands:
   - `cocoon daemon start [--listen unix:///var/run/cocoon/cocoon.sock]`
   - `cocoon daemon stop`
   - `cocoon daemon status`
5. Register daemon subcommand in `cmd/root.go`

### Phase 5: Daemon integration tests

**Goal**: Verify daemon serves requests correctly.

**Steps**:

1. Create `daemon/daemon_test.go`
2. Start daemon in-process with test config (temp dirs)
3. Connect gRPC client
4. Run same scenarios as service-layer tests but through gRPC
5. Verify: request → gRPC → service → mock backend → gRPC response → client

Key test cases:
- VM CRUD lifecycle through gRPC
- Console bidirectional streaming
- Pull with progress streaming
- Concurrent requests (verify no deadlocks)
- Graceful shutdown (in-flight requests complete)

---

## 7. File-by-File Change Spec

### New files

| File | Phase | Content |
|------|-------|---------|
| `service/params.go` | 1 | Parameter structs (VMCreateParams, etc.) |
| `service/service.go` | 1 | Service struct, New(), NewWithBackends() |
| `service/vm.go` | 2 | VM business logic |
| `service/image.go` | 2 | Image business logic |
| `service/snapshot.go` | 2 | Snapshot business logic |
| `service/gc.go` | 2 | GC logic |
| `service/testutil_test.go` | 1 | Mock backends |
| `service/vm_test.go` | 1 | VM service tests |
| `service/image_test.go` | 1 | Image service tests |
| `service/snapshot_test.go` | 1 | Snapshot service tests |
| `service/gc_test.go` | 1 | GC service tests |
| `test/integration/cli_test.go` | 3 | CLI smoke tests |
| `proto/cocoon/v1/*.proto` | 4 | gRPC definitions |
| `daemon/daemon.go` | 4 | Daemon struct, Start/Stop |
| `daemon/vm.go` | 4 | gRPC VM service impl |
| `daemon/image.go` | 4 | gRPC Image service impl |
| `daemon/snapshot.go` | 4 | gRPC Snapshot service impl |
| `daemon/gc.go` | 4 | gRPC GC service impl |
| `daemon/daemon_test.go` | 5 | Daemon integration tests |
| `cmd/daemon/commands.go` | 4 | Daemon CLI commands |
| `cmd/daemon/handler.go` | 4 | Daemon CLI handler |

### Modified files

| File | Phase | Changes |
|------|-------|---------|
| `cmd/vm/handler.go` | 2 | Replace logic with service calls; keep flag parsing + output |
| `cmd/images/handler.go` | 2 | Same |
| `cmd/snapshot/handler.go` | 2 | Same |
| `cmd/others/handler.go` | 2 | Same |
| `cmd/core/helpers.go` | 2 | Add `*ParamsFromFlags()` functions; Service factory |
| `cmd/root.go` | 4 | Register daemon subcommand; add `--host` flag |
| `go.mod` | 4 | Add `google.golang.org/grpc`, `google.golang.org/protobuf` |
| `Makefile` | 4 | Add `proto` target for code generation |

### Unchanged files

Everything in `hypervisor/`, `images/`, `network/`, `snapshot/`, `storage/`,
`lock/`, `types/`, `metadata/`, `gc/`, `utils/`, `config/`, `console/`,
`progress/`, `version/`.

---

## 8. Testing Strategy

### Test pyramid

```
         /  E2E (daemon + CLI)  \        ← Phase 5: few, slow
        /  CLI smoke tests       \       ← Phase 3: few, need KVM
       /  Service-layer unit tests\      ← Phase 1-2: many, fast, mock backends
      /  Backend unit tests (existing)\  ← Already exist (utils, snapshot, etc.)
```

### Mock backend design

```go
// service/testutil_test.go

type mockHypervisor struct {
    // Each method has a corresponding function field.
    // Tests set these to control behavior.
    createFn  func(ctx context.Context, id string, cfg *types.VMConfig, ...) (*types.VM, error)
    startFn   func(ctx context.Context, refs []string) ([]string, error)
    stopFn    func(ctx context.Context, refs []string) ([]string, error)
    deleteFn  func(ctx context.Context, refs []string, force bool) ([]string, error)
    inspectFn func(ctx context.Context, ref string) (*types.VM, error)
    listFn    func(ctx context.Context) ([]*types.VM, error)
    consoleFn func(ctx context.Context, ref string) (io.ReadWriteCloser, error)
    // ... snapshot, clone, restore
}

func (m *mockHypervisor) Create(ctx context.Context, id string, ...) (*types.VM, error) {
    if m.createFn != nil {
        return m.createFn(ctx, id, ...)
    }
    return &types.VM{ID: id, State: types.VMStateCreated}, nil
}
// ... other methods follow the same pattern
```

This pattern allows each test to configure exactly the behavior it needs
without creating separate mock types for each scenario.

### Test data conventions

- Use `t.TempDir()` for all file system state
- Build `config.Config` with temp dirs for rootDir, runDir, logDir
- Mock backends return canned data; never touch the filesystem
- Table-driven tests for all validation logic (params validation, resource minimums)

### Running tests

```bash
# Unit tests (fast, no KVM needed) — this is what CI runs
make test

# Integration tests (need KVM + built binary)
go test -tags integration -v ./test/integration/

# All tests
make test && go test -tags integration -v ./test/integration/
```

---

## 9. Migration Safety Rules

These rules **must** be followed during Phase 2 (service extraction) to ensure
correctness:

### R1: One method at a time

Extract one handler method → one service method. After each extraction:
1. `go build ./...` passes
2. `go vet ./...` passes
3. Service tests for that method pass
4. No other handler changed in the same commit

### R2: Exact logic preservation

When moving code from handler to service:
- Copy the logic **verbatim** first
- Replace cobra flag reads with params struct field access
- Replace `fmt.Println` / `logger.Infof` with return values
- Do NOT refactor, optimize, or "improve" the logic during the move
- Do NOT change error messages (they may be matched by scripts)

### R3: Param-to-VMConfig conversion in service layer

The service layer converts `VMCreateParams` → `*types.VMConfig` internally:

```go
func (p VMCreateParams) toVMConfig() *types.VMConfig {
    return &types.VMConfig{
        Name:    p.Name,
        CPU:     p.CPU,
        Memory:  p.Memory,
        Storage: p.Storage,
        Image:   p.Image,
        Network: p.Network,
    }
}
```

For Clone/Restore, the merge logic from `CloneVMConfigFromFlags` /
`RestoreVMConfigFromFlags` moves into the service method (it's business logic,
not flag parsing).

### R4: No backend interface changes

If a service method needs something a backend doesn't provide, do NOT change
the backend interface. Instead, compose existing interface methods.

### R5: Error propagation

Service methods wrap errors with the same patterns as current handlers:
- `fmt.Errorf("create VM: %w", err)` — not `fmt.Errorf("CreateVM: %w", err)`
- Keep lowercase error prefixes
- Preserve `%w` wrapping for `errors.Is` / `errors.As` compatibility

### R6: Context propagation

Every service method takes `context.Context` as first parameter and passes it
through to all backend calls. Never create a new context inside a service method.

### R7: Network rollback preservation

The current `rollbackNetwork` pattern must be preserved exactly:
- If `hyper.Create` fails after `network.Config` succeeds → rollback network
- If `hyper.Clone` fails after network setup → rollback network
- The rollback is best-effort (log warning on failure, don't return error)

### R8: Direct/Stream dual-path preservation

Clone and Restore both have a Direct fast path + Stream fallback. This logic
moves to the service layer with the exact same type-assertion pattern:

```go
if da, ok := s.snapshot.(snapshot.Direct); ok {
    if dcr, ok := s.hypervisor.(hypervisor.Direct); ok {
        return s.cloneDirect(ctx, dcr, da, ...)
    }
}
// fallback to stream
```

### R9: Progress tracker passthrough

Image Pull/Import methods accept `progress.Tracker` as a parameter. The service
layer passes it through to the backend. It does NOT create trackers — that's the
caller's responsibility (CLI creates terminal-printing trackers, daemon creates
gRPC-streaming trackers).

### R10: Commit discipline

Each phase is a series of small, reviewable commits:
- Phase 1: "add service params", "add mock backends", "add VM service tests", ...
- Phase 2: "extract CreateVM to service", "extract RunVM to service", ...
- Phase 3: "add CLI integration smoke tests"
- Phase 4: "add protobuf definitions", "implement daemon server", ...
- Phase 5: "add daemon integration tests"

Never mix extraction of different methods in one commit.

---

## 10. Code Style Rules

All new and modified code in this project **must** follow these formatting rules.

### S1: Blank-line separation between logical blocks

Use a blank line to separate each logical block within a function. A "logical block"
is typically one operation and its error check, or a group of closely related statements.

**Bad** (wall of code, no visual separation):

```go
func New(ctx context.Context, conf *config.Config) (*Service, error) {
    ociStore, err := oci.New(ctx, conf)
    if err != nil {
        return nil, fmt.Errorf("init oci backend: %w", err)
    }
    cloudimgStore, err := cloudimg.New(ctx, conf)
    if err != nil {
        return nil, fmt.Errorf("init cloudimg backend: %w", err)
    }
    hyper, err := cloudhypervisor.New(conf)
    if err != nil {
        return nil, fmt.Errorf("init hypervisor: %w", err)
    }
    return &Service{hypervisor: hyper}, nil
}
```

**Good** (each init+err is its own visual block):

```go
func New(ctx context.Context, conf *config.Config) (*Service, error) {
    ociStore, err := oci.New(ctx, conf)
    if err != nil {
        return nil, fmt.Errorf("init oci backend: %w", err)
    }

    cloudimgStore, err := cloudimg.New(ctx, conf)
    if err != nil {
        return nil, fmt.Errorf("init cloudimg backend: %w", err)
    }

    hyper, err := cloudhypervisor.New(conf)
    if err != nil {
        return nil, fmt.Errorf("init hypervisor: %w", err)
    }

    return &Service{hypervisor: hyper}, nil
}
```

### S2: Handlers use three clearly separated sections

After refactoring, each CLI handler should have three visually distinct sections
separated by blank lines, each preceded by a comment:

```go
func (h Handler) Run(cmd *cobra.Command, args []string) error {
    ctx, svc, err := h.initService(cmd)
    if err != nil {
        return err
    }

    // Step 1: Parse flags -> params (CLI-specific)
    params, err := vmCreateParamsFromFlags(cmd, args[0])
    if err != nil {
        return err
    }

    // Step 2: Call service (shared with daemon)
    vm, err := svc.RunVM(ctx, params)
    if err != nil {
        return err
    }

    // Step 3: Format output (CLI-specific)
    logger := log.WithFunc("cmd.run")
    logger.Infof(ctx, "VM created: %s (name: %s)", vm.ID, vm.Config.Name)
    logger.Infof(ctx, "started: %s", vm.ID)
    return nil
}
```

### S3: General principle

One blank line = one breath. Each block of code that does one logical thing
(initialize something, validate something, call something, format something)
should be separated from the next by a blank line. When scanning the function,
the blank lines should reveal the structure at a glance.
