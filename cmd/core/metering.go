package core

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/metering"
	meteringfile "github.com/cocoonstack/cocoon/metering/file"
	"github.com/cocoonstack/cocoon/metering/metalog"
	meteringstderr "github.com/cocoonstack/cocoon/metering/stderr"
)

const (
	meteringSubdir = "metering"
	meteringFile   = "ledger.jsonl"
)

var (
	meteringOnce sync.Once
	meteringRec  metering.Recorder
)

// MeteringRecorder returns the process-wide recorder per conf.Metering.Backend; lazy-init shared across callers.
func MeteringRecorder(ctx context.Context, conf *config.Config) metering.Recorder {
	meteringOnce.Do(func() {
		meteringRec = buildRecorder(ctx, conf)
	})
	return meteringRec
}

func buildRecorder(ctx context.Context, conf *config.Config) metering.Recorder {
	logger := log.WithFunc("core.buildRecorder")
	switch conf.Metering.Backend {
	case "", config.MeteringFile:
		return buildFileRecorder(ctx, conf)
	case config.MeteringNop:
		return metering.NopRecorder{}
	case config.MeteringStderr:
		return meteringstderr.New()
	case config.MeteringMeta:
		// The json engine writes the metering namespace under this dir.
		if err := os.MkdirAll(filepath.Join(conf.RootDir, meteringSubdir), 0o750); err != nil {
			logger.Warnf(ctx, "mkdir metering dir: %v; metering disabled", err)
			return metering.NopRecorder{}
		}
		s, err := MetaStore(conf)
		if err != nil {
			logger.Warnf(ctx, "meta store: %v; metering disabled", err)
			return metering.NopRecorder{}
		}
		return metalog.New(s)
	default:
		logger.Warnf(ctx, "unknown metering backend %q; using nop", conf.Metering.Backend)
		return metering.NopRecorder{}
	}
}

func buildFileRecorder(ctx context.Context, conf *config.Config) metering.Recorder {
	logger := log.WithFunc("core.buildFileRecorder")
	path := conf.Metering.File.Path
	if path == "" {
		path = filepath.Join(conf.RootDir, meteringSubdir, meteringFile)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		logger.Warnf(ctx, "mkdir %s: %v; metering disabled", filepath.Dir(path), err)
		return metering.NopRecorder{}
	}
	r, err := meteringfile.New(path)
	if err != nil {
		logger.Warnf(ctx, "open ledger: %v; metering disabled", err)
		return metering.NopRecorder{}
	}
	return r
}
