package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/projecteru2/cocoon/config"
	imagebackend "github.com/projecteru2/cocoon/images"
	"github.com/projecteru2/cocoon/images/cloudimg"
	"github.com/projecteru2/cocoon/types"
)

// resolveImage resolves an image reference to StorageConfigs + BootConfig
// by trying each backend in order.
func resolveImage(ctx context.Context, backends []imagebackend.Images, vmCfg *types.VMConfig) ([]*types.StorageConfig, *types.BootConfig, error) {
	vms := []*types.VMConfig{vmCfg}

	var storageConfigs []*types.StorageConfig
	var bootCfg *types.BootConfig
	var backendErrs []string

	for _, b := range backends {
		confs, boots, err := b.Config(ctx, vms)
		if err != nil {
			backendErrs = append(backendErrs, fmt.Sprintf("%s: %v", b.Type(), err))
			continue
		}

		storageConfigs = confs[0]
		bootCfg = boots[0]
		break
	}

	if bootCfg == nil {
		return nil, nil, fmt.Errorf("image %q not resolved: %s", vmCfg.Image, strings.Join(backendErrs, "; "))
	}

	return storageConfigs, bootCfg, nil
}

// ensureFirmwarePath sets default firmware path for cloudimg boot
// when no kernel is specified.
func ensureFirmwarePath(conf *config.Config, bootCfg *types.BootConfig) {
	if bootCfg != nil && bootCfg.KernelPath == "" && bootCfg.FirmwarePath == "" {
		bootCfg.FirmwarePath = cloudimg.NewConfig(conf).FirmwarePath()
	}
}

// IsURL returns true if the reference looks like an HTTP(S) URL.
func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}
