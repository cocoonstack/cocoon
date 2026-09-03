package cmd

import (
	"testing"

	"github.com/spf13/viper"

	"github.com/cocoonstack/cocoon/config"
)

func TestEnvOverridesDottedLogLevel(t *testing.T) {
	t.Setenv("COCOON_LOG_LEVEL", "debug")
	viper.Reset()
	newRootCmd()
	if err := initConfig(t.Context()); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if conf.Log.Level != "debug" {
		t.Fatalf("log level: got %q, want %q", conf.Log.Level, "debug")
	}
}

func TestLogRotationKeysDecode(t *testing.T) {
	v := viper.New()
	v.SetDefault("log.max_size", 7)
	v.SetDefault("log.max_age", 3)
	v.SetDefault("log.max_backups", 2)

	cfg := &config.Config{}
	if err := v.Unmarshal(cfg, matchSnakeCase); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Log.MaxSize != 7 || cfg.Log.MaxAge != 3 || cfg.Log.MaxBackups != 2 {
		t.Fatalf("log rotation: got %+v, want max_size=7 max_age=3 max_backups=2", cfg.Log)
	}
}
