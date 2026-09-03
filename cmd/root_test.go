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
