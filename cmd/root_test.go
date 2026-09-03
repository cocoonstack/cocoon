package cmd

import (
	"testing"

	"github.com/spf13/viper"
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
	t.Setenv("COCOON_LOG_MAXAGE", "7")
	viper.Reset()
	newRootCmd()
	if err := initConfig(t.Context()); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if conf.Log.MaxSize != 500 || conf.Log.MaxAge != 7 || conf.Log.MaxBackups != 3 {
		t.Fatalf("log rotation: got %+v, want maxsize=500 maxage=7 maxbackups=3", conf.Log)
	}
}
