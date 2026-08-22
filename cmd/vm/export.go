package vm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/progress"
)

func (h Handler) Export(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
	vmRef, destination := args[0], args[1]
	target, err := cmdcore.ParseOCIPushTarget(destination)
	if err != nil {
		return err
	}
	hyper, err := cmdcore.FindHypervisor(ctx, conf, vmRef)
	if err != nil {
		return fmt.Errorf("find VM %s: %w", vmRef, err)
	}
	exporter, ok := hyper.(hypervisor.CloudImageExporter)
	if !ok {
		return fmt.Errorf("hypervisor %s does not support cloud-image export", hyper.Type())
	}
	tmp, err := os.CreateTemp(conf.RootDir, ".cocoon-export-*.qcow2")
	if err != nil {
		return fmt.Errorf("create export temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close export temp file: %w", err)
	}
	defer os.Remove(tmpPath) //nolint:errcheck

	logger := log.WithFunc("cmd.vm.Export")
	logger.Infof(ctx, "flattening VM %s ...", vmRef)
	vm, err := exporter.ExportCloudImage(ctx, vmRef, tmpPath)
	if err != nil {
		return fmt.Errorf("export VM %s: %w", vmRef, err)
	}

	osName := "linux"
	if vm.Config.Windows {
		osName = "windows"
	}
	logger.Infof(ctx, "pushing VM %s to %s ...", vmRef, destination)
	result, err := (&cloudimg.Pusher{Uploader: target.Registry}).Push(ctx, cloudimg.PushOptions{
		Name:  target.Repository,
		Tag:   target.Tag,
		Path:  tmpPath,
		Title: filepath.Base(target.Repository) + ".qcow2",
		Annotations: map[string]string{
			"cocoonstack.os.name":   osName,
			"cocoonstack.source.vm": vm.ID,
		},
	})
	if err != nil {
		return fmt.Errorf("push cloud image: %w", err)
	}
	localName, _ := cmd.Flags().GetString("local-name")
	if localName != "" {
		_, store, initErr := cmdcore.InitImageBackendsForPull(ctx, conf)
		if initErr != nil {
			return fmt.Errorf("pushed %s as %s, but initializing the local cloud-image store failed: %w", destination, result.ManifestDigest, initErr)
		}
		logger.Infof(ctx, "retaining exported VM as local image %s ...", localName)
		if importErr := store.Import(ctx, localName, progress.Nop, tmpPath); importErr != nil {
			return fmt.Errorf("pushed %s as %s, but retaining local cloud image %s failed: %w", destination, result.ManifestDigest, localName, importErr)
		}
	}
	fmt.Println(result.ManifestDigest)
	return nil
}
