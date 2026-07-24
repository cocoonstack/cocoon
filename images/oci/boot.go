package oci

import (
	"archive/tar"
	"bufio"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/projecteru2/core/log"
)

// maxKernelBytes bounds arm64 gzip decompression against a bomb; real kernels are under 100 MiB.
const maxKernelBytes = 512 << 20

func bootFilesPresent(results []pullLayerResult) (hasKernel, hasInitrd bool) {
	for i := range results {
		if results[i].kernelPath != "" {
			hasKernel = true
		}
		if results[i].initrdPath != "" {
			hasInitrd = true
		}
		if hasKernel && hasInitrd {
			return hasKernel, hasInitrd
		}
	}
	return hasKernel, hasInitrd
}

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
		results[i].kernelPath = cmp.Or(results[i].kernelPath, kp)
		results[i].initrdPath = cmp.Or(results[i].initrdPath, ip)
	}
}

func selfHealBootFiles(ctx context.Context, j layerJob, digestHex string) {
	logger := log.WithFunc("oci.selfHealBootFiles")

	if j.result.kernelPath != "" && j.result.initrdPath != "" {
		return
	}

	hasEvidence := j.result.kernelPath != "" || j.result.initrdPath != ""
	if !hasEvidence {
		_, statErr := os.Stat(j.conf.BootDir(digestHex))
		hasEvidence = statErr == nil
	}
	if !hasEvidence {
		_, hasEvidence = j.knownBootHexes[digestHex]
	}
	if !hasEvidence {
		return
	}

	logger.Warnf(ctx, "Layer %d: sha256:%s attempting boot file recovery", j.idx, digestHex[:12])
	kp, ip := recoverBootFiles(ctx, j.layer, j.workDir, j.idx, digestHex)
	j.result.kernelPath = cmp.Or(j.result.kernelPath, kp)
	j.result.initrdPath = cmp.Or(j.result.initrdPath, ip)
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

func scanBootFiles(ctx context.Context, r io.Reader, workDir, namePrefix string) (kernelPath, initrdPath string, err error) {
	logger := log.WithFunc("oci.scanBootFiles")

	tr := tar.NewReader(r)
	for {
		hdr, readErr := tr.Next()
		if errors.Is(readErr, io.EOF) {
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
			dstPath = filepath.Join(workDir, namePrefix+".vmlinuz")
		} else {
			dstPath = filepath.Join(workDir, namePrefix+".initrd.img")
		}

		f, createErr := os.Create(dstPath) //nolint:gosec
		if createErr != nil {
			return "", "", fmt.Errorf("create %s: %w", filepath.Base(dstPath), createErr)
		}
		// CH on arm64 direct-boots only a raw kernel Image, but Ubuntu ships the
		// arm64 vmlinuz gzip-compressed; decompress it (x86 bzImage is not gzip).
		src := io.Reader(tr)
		var gz *gzip.Reader
		if isKernel && runtime.GOARCH == "arm64" {
			br := bufio.NewReader(tr)
			src = br
			if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
				var gzErr error
				if gz, gzErr = gzip.NewReader(br); gzErr != nil {
					_ = f.Close()
					return "", "", fmt.Errorf("gunzip %s: %w", filepath.Base(dstPath), gzErr)
				}
				src = io.LimitReader(gz, maxKernelBytes+1)
			}
		}
		written, copyErr := io.Copy(f, src) //nolint:gosec
		if gz != nil {
			_ = gz.Close()
		}
		_ = f.Close()
		if copyErr != nil {
			return "", "", fmt.Errorf("write %s: %w", filepath.Base(dstPath), copyErr)
		}
		if gz != nil && written > maxKernelBytes {
			return "", "", fmt.Errorf("decompressed kernel %s exceeds %d bytes", filepath.Base(dstPath), maxKernelBytes)
		}

		if isKernel {
			kernelPath = dstPath
		} else {
			initrdPath = dstPath
		}
		// namePrefix is a short placeholder on the import path, not always a digest.
		logger.Debugf(ctx, "Layer %s: extracted %s", namePrefix[:min(len(namePrefix), 12)], base)
	}
	return kernelPath, initrdPath, nil
}
