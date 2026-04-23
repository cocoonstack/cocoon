package oci

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/projecteru2/core/log"
)

func healCachedBootFiles(ctx context.Context, conf *Config, layers []v1.Layer, results []pullLayerResult, workDir string) {
	logger := log.WithFunc("oci.healCachedBootFiles")

	hasKernel, hasInitrd := bootFilesPresent(results)
	if hasKernel && hasInitrd {
		return
	}

	logger.Warnf(ctx, "Boot files incomplete after first pass (kernel=%v, initrd=%v), force-scanning cached layers", hasKernel, hasInitrd)

	for i, layer := range layers {
		digestHex := results[i].digest.Hex()
		if results[i].erofsPath != conf.BlobPath(digestHex) {
			continue
		}
		kp, ip := recoverBootFiles(ctx, layer, workDir, i, digestHex)
		if results[i].kernelPath == "" && kp != "" {
			results[i].kernelPath = kp
		}
		if results[i].initrdPath == "" && ip != "" {
			results[i].initrdPath = ip
		}
	}
}

func selfHealBootFiles(ctx context.Context, conf *Config, layer v1.Layer, workDir string, idx int, digestHex string, knownBootHexes map[string]struct{}, result *pullLayerResult) {
	if result.kernelPath != "" && result.initrdPath != "" {
		return
	}

	hasEvidence := result.kernelPath != "" || result.initrdPath != ""
	if !hasEvidence {
		_, statErr := os.Stat(conf.BootDir(digestHex))
		hasEvidence = statErr == nil
	}
	if !hasEvidence {
		_, hasEvidence = knownBootHexes[digestHex]
	}
	if !hasEvidence {
		return
	}

	log.WithFunc("oci.processLayer").Warnf(ctx, "Layer %d: sha256:%s attempting boot file recovery", idx, digestHex[:12])
	kp, ip := recoverBootFiles(ctx, layer, workDir, idx, digestHex)
	if result.kernelPath == "" {
		result.kernelPath = kp
	}
	if result.initrdPath == "" {
		result.initrdPath = ip
	}
}

func recoverBootFiles(ctx context.Context, layer v1.Layer, workDir string, idx int, digestHex string) (kernelPath, initrdPath string) {
	logger := log.WithFunc("oci.recoverBootFiles")
	healDir := filepath.Join(workDir, fmt.Sprintf("heal-%d", idx))
	if err := os.MkdirAll(healDir, 0o750); err != nil {
		logger.Warnf(ctx, "Layer %d: cannot create heal dir: %v", idx, err)
		return "", ""
	}
	rc, err := layer.Uncompressed()
	if err != nil {
		logger.Warnf(ctx, "Layer %d: cannot open for boot scan: %v", idx, err)
		return "", ""
	}
	kp, ip, scanErr := scanBootFiles(ctx, rc, healDir, digestHex)
	_ = rc.Close()
	if scanErr != nil {
		logger.Warnf(ctx, "Layer %d: boot scan failed: %v", idx, scanErr)
		return "", ""
	}
	return kp, ip
}

func scanBootFiles(ctx context.Context, r io.Reader, workDir, digestHex string) (kernelPath, initrdPath string, err error) {
	logger := log.WithFunc("oci.scanBootFiles")

	tr := tar.NewReader(r)
	for {
		hdr, readErr := tr.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", "", fmt.Errorf("read tar entry: %w", readErr)
		}

		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // tar.TypeRegA is deprecated but still present in real archives
			continue
		}

		entryName := filepath.Clean(hdr.Name)
		base := filepath.Base(entryName)

		if strings.HasSuffix(base, ".old") {
			continue
		}

		isKernel := strings.HasPrefix(base, "vmlinuz")
		isInitrd := strings.HasPrefix(base, "initrd.img")
		if !isKernel && !isInitrd {
			continue
		}

		dir := filepath.Dir(entryName)
		if dir != "boot" && dir != "." {
			continue
		}

		var dstPath string
		if isKernel {
			dstPath = filepath.Join(workDir, digestHex+".vmlinuz")
		} else {
			dstPath = filepath.Join(workDir, digestHex+".initrd.img")
		}

		f, createErr := os.Create(dstPath) //nolint:gosec
		if createErr != nil {
			return "", "", fmt.Errorf("create %s: %w", filepath.Base(dstPath), createErr)
		}
		if _, copyErr := io.Copy(f, tr); copyErr != nil { //nolint:gosec
			_ = f.Close()
			return "", "", fmt.Errorf("write %s: %w", filepath.Base(dstPath), copyErr)
		}
		_ = f.Close()

		if isKernel {
			kernelPath = dstPath
		} else {
			initrdPath = dstPath
		}
		logger.Debugf(ctx, "Layer %s: extracted %s", digestHex[:12], base)
	}
	return kernelPath, initrdPath, nil
}
