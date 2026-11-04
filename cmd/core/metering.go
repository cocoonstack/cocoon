package core

import (
	"cmp"
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

// MeteringRecorder returns the process-wide recorder for conf.Metering.Backend.
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
	case config.MeteringStderr:
		return meteringstderr.New()
	case config.MeteringMeta:
		s, err := MetaStore(conf)
		if err != nil {
			logger.Warnf(ctx, "meta store: %v; metering disabled", err)
			return metering.NopRecorder{}
		}
		return metalog.New(s)
	default:
		return metering.NopRecorder{}
	}
}

func buildFileRecorder(ctx context.Context, conf *config.Config) metering.Recorder {
	logger := log.WithFunc("core.buildFileRecorder")
	path := cmp.Or(conf.Metering.File.Path, filepath.Join(conf.RootDir, meteringSubdir, meteringFile))
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
