package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	imagebackend "github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func FindHypervisor(ctx context.Context, conf *config.Config, ref string) (hypervisor.Hypervisor, error) {
	hypers, err := InitAllHypervisors(ctx, conf)
	if err != nil {
		return nil, err
	}
	owner, _, err := resolveVMOwner(ctx, hypers, ref)
	return owner, err
}

func ListAllVMs(ctx context.Context, hypers []hypervisor.Hypervisor) ([]*types.VM, error) {
	var all []*types.VM
	for _, h := range hypers {
		vms, listErr := h.List(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list %s: %w", h.Type(), listErr)
		}
		all = append(all, vms...)
	}
	return all, nil
}

// RouteRefs resolves user refs to (hypervisor → full VM IDs).
func RouteRefs(ctx context.Context, hypers []hypervisor.Hypervisor, refs []string) (map[hypervisor.Hypervisor][]string, error) {
	result := map[hypervisor.Hypervisor][]string{}
	for _, ref := range refs {
		owner, vm, err := resolveVMOwner(ctx, hypers, ref)
		if err != nil {
			return nil, err
		}
		result[owner] = append(result[owner], vm.ID)
	}
	return result, nil
}

func ResolveImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) ([]*types.StorageConfig, *types.BootConfig, error) {
	vms := []*types.VMConfig{vmCfg}
	var owner imagebackend.Images
	var storageConfigs []*types.StorageConfig
	var bootCfg *types.BootConfig
	var backendErrs []string
	for _, b := range backends {
		confs, boots, err := b.Config(ctx, vms)
		if err != nil {
			backendErrs = append(backendErrs, fmt.Sprintf("%s: %v", b.Type(), err))
			continue
		}
		if owner != nil {
			return nil, nil, fmt.Errorf("image %s: %w (matched both %s and %s)",
				vmCfg.Image, imagebackend.ErrAmbiguous, owner.Type(), b.Type())
		}
		owner = b
		storageConfigs = confs[0]
		bootCfg = boots[0]
	}
	if owner == nil {
		return nil, nil, fmt.Errorf("image %q not resolved: %s", vmCfg.Image, strings.Join(backendErrs, "; "))
	}
	return storageConfigs, bootCfg, nil
}

// EnsureImage pulls the digest-pinned base if missing; only warns so VerifyBaseFiles surfaces the real error for imported images.
func EnsureImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) {
	if vmCfg.Image == "" || vmCfg.ImageType == "" {
		return
	}
	logger := log.WithFunc("core.EnsureImage")

	lookupRef := cmp.Or(vmCfg.ImageDigest, vmCfg.Image)

	for _, b := range backends {
		if b.Type() != vmCfg.ImageType {
			continue
		}
		img, inspectErr := b.Inspect(ctx, lookupRef)
		if inspectErr != nil {
			logger.Warnf(ctx, "inspect image %s: %v — will attempt pull", lookupRef, inspectErr)
		}
		if img != nil {
			return // exact image version exists locally
		}
		// Pull by digest when available — the tag may point at a different manifest than at snapshot time.
		pullRef := digestPullRef(vmCfg.Image, vmCfg.ImageDigest, vmCfg.ImageType)
		if shapeErr := validateRefShape(pullRef, vmCfg.ImageType); shapeErr != nil {
			logger.Warnf(ctx, "skipping auto-pull of %s: %v — pre-pull manually if missing", pullRef, shapeErr)
			return
		}
		logger.Infof(ctx, "base image not found locally, pulling %s ...", pullRef)
		// Pinned digest with no local hit: force past cloudimg's URL-keyed cache.
		needForce := vmCfg.ImageDigest != ""
		if pullErr := b.Pull(ctx, pullRef, needForce, progress.Nop); pullErr != nil {
			logger.Warnf(ctx, "auto-pull %s failed (imported image?): %v — clone may fail if base layers are missing", pullRef, pullErr)
			return
		}
		// cloudimg has no digest pinning, so a pull may drift; verify the digest after.
		if vmCfg.ImageDigest != "" && pullRef == vmCfg.Image {
			img, err := b.Inspect(ctx, vmCfg.ImageDigest)
			if err != nil || img == nil {
				logger.Warnf(ctx, "pulled %s but expected digest %s not found locally — clone may fail in VerifyBaseFiles", pullRef, vmCfg.ImageDigest)
				return
			}
		}
		logger.Infof(ctx, "base image %s pulled successfully", pullRef)
		return
	}
}

func ResolveImageOwner(ctx context.Context, backends []imagebackend.Images, ref string) (imagebackend.Images, error) {
	return resolveOwner(backends, ref, func(b imagebackend.Images) (bool, error) {
		img, err := b.Inspect(ctx, ref)
		return img != nil, err
	},
		fmt.Errorf("image %q: not found in any backend", ref),
		fmt.Errorf("image %s: %w", ref, imagebackend.ErrAmbiguous),
	)
}

func ReconcileState(vm *types.VM) string {
	if vm.State == types.VMStateRunning && !utils.IsProcessAlive(vm.PID) {
		return "stopped (stale)"
	}
	return string(vm.State)
}

// CloseOnCancel closes c when ctx is canceled; callers `defer CloseOnCancel(ctx, c)()` to stop the watcher on return.
func CloseOnCancel(ctx context.Context, c io.Closer) func() bool {
	return context.AfterFunc(ctx, func() {
		c.Close() //nolint:errcheck,gosec
	})
}

// resolveOwner returns the unique backend where found==true; notFound on zero, ambiguous wrapped on multi-match (lists matched types).
func resolveOwner[T interface{ Type() string }](backends []T, ref string, found func(T) (bool, error), notFound, ambiguous error) (T, error) {
	var matches []T
	var zero T
	for _, b := range backends {
		ok, err := found(b)
		if err != nil {
			return zero, fmt.Errorf("inspect %s in %s: %w", ref, b.Type(), err)
		}
		if ok {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 0:
		return zero, notFound
	case 1:
		return matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, b := range matches {
			names[i] = b.Type()
		}
		return zero, fmt.Errorf("%w (backends: %s)", ambiguous, strings.Join(names, ", "))
	}
}

// validateRefShape rejects URL/OCI ref mismatches early so backends don't surface misleading downstream errors.
func validateRefShape(ref, imageType string) error {
	switch imageType {
	case types.ImageTypeCloudImg:
		if !cliutil.IsURL(ref) {
			return fmt.Errorf("cloudimg ref %q is not an http(s) URL (imported or bare OCI ref?)", ref)
		}
	case types.ImageTypeOCI:
		if _, err := name.ParseReference(ref); err != nil {
			return fmt.Errorf("oci ref %q is not a valid OCI reference: %w", ref, err)
		}
	}
	return nil
}

// digestPullRef pins OCI pulls by digest; returns image as-is for others.
func digestPullRef(image, digest, imageType string) string {
	if digest == "" || imageType != types.ImageTypeOCI {
		return image
	}
	// OCI: convert "registry/repo:tag" → "registry/repo@sha256:..."
	ref, err := name.ParseReference(image)
	if err != nil {
		return image
	}
	return ref.Context().String() + "@" + digest
}

// resolveVMOwner returns the owning hypervisor and resolved *types.VM so callers use vm.ID instead of re-resolving the raw ref.
func resolveVMOwner(ctx context.Context, hypers []hypervisor.Hypervisor, ref string) (hypervisor.Hypervisor, *types.VM, error) {
	var resolved *types.VM
	owner, err := resolveOwner(hypers, ref, func(h hypervisor.Hypervisor) (bool, error) {
		vm, err := h.Inspect(ctx, ref)
		if err == nil && vm != nil {
			resolved = vm
			return true, nil
		}
		if err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
			return false, err
		}
		return false, nil
	},
		fmt.Errorf("vm %s: %w", ref, hypervisor.ErrNotFound),
		fmt.Errorf("vm %s: %w", ref, hypervisor.ErrAmbiguous),
	)
	return owner, resolved, err
}

// EnsureSnapshotNameFree validates name and rejects a duplicate; empty passes.
func EnsureSnapshotNameFree(ctx context.Context, snapBackend snapshot.Snapshot, name string) error {
	if err := (&types.SnapshotConfig{Name: name}).Validate(); err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	if _, err := snapBackend.Inspect(ctx, name); err == nil {
		return fmt.Errorf("snapshot name %q already exists", name)
	} else if !errors.Is(err, snapshot.ErrNotFound) {
		return fmt.Errorf("check snapshot name: %w", err)
	}
	return nil
}

// CaptureSnapshot checks the --name preflight, runs capture, and persists the capture dir, returning the stored snapshot id.
func CaptureSnapshot(ctx context.Context, cmd *cobra.Command, snapBackend snapshot.Snapshot, capture func() (*types.SnapshotConfig, string, error)) (string, error) {
	name, _ := cmd.Flags().GetString("name")
	description, _ := cmd.Flags().GetString("description")
	if err := EnsureSnapshotNameFree(ctx, snapBackend, name); err != nil {
		return "", err
	}
	cfg, srcDir, err := capture()
	if err != nil {
		return "", err
	}
	return PersistSnapshotDir(ctx, snapBackend, cfg, srcDir, name, description)
}

// PersistSnapshotDir stores a finalized capture dir, preferring a direct in-place move (DirectCreator) when srcDir shares a filesystem with the backend's data dir, and falling back to a tar stream otherwise (cross-filesystem, or a backend without DirectCreator such as a remote store). srcDir is consumed on every path.
func PersistSnapshotDir(ctx context.Context, snapBackend snapshot.Snapshot, cfg *types.SnapshotConfig, srcDir, name, description string) (string, error) {
	cfg.Name = name
	cfg.Description = description
	if dc, ok := snapBackend.(snapshot.DirectCreator); ok {
		id, done, err := dc.CreateFromDir(ctx, cfg, srcDir)
		if err != nil {
			os.RemoveAll(srcDir) //nolint:errcheck,gosec // consume srcDir on every path: a failed direct save must not leave the GB capture dir behind
			return "", fmt.Errorf("save snapshot: %w", err)
		}
		if done {
			log.WithFunc("core.PersistSnapshotDir").Info(ctx, "saved snapshot data (direct)")
			return id, nil
		}
	}
	return PersistSnapshotStream(ctx, snapBackend, cfg, utils.TarDirStreamWithRemove(srcDir), name, description)
}

// PersistSnapshotStream labels cfg and stores the stream, closing it either way.
func PersistSnapshotStream(ctx context.Context, snapBackend snapshot.Snapshot, cfg *types.SnapshotConfig, stream io.ReadCloser, name, description string) (string, error) {
	defer stream.Close() //nolint:errcheck
	defer CloseOnCancel(ctx, stream)()
	cfg.Name = name
	cfg.Description = description
	log.WithFunc("core.PersistSnapshotStream").Info(ctx, "saving snapshot data ...")
	snapID, err := snapBackend.Create(ctx, cfg, stream)
	if err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}
	return snapID, nil
}
