package cmd

import (
	"cmp"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	coretypes "github.com/projecteru2/core/types"
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
	if logConfig(t).Level != "debug" {
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
	if log := logConfig(t); log.MaxSize != 500 || log.MaxAge != 7 || log.MaxBackups != 3 {
		t.Fatalf("log rotation: got %+v, want maxsize=500 maxage=7 maxbackups=3", conf.Log)
	}
}

func TestConfigFileLogSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cocoon.yaml")
	logPath := filepath.Join(dir, "cocoon.log")
	body := "log:\n  level: warn\n  filename: " + logPath + "\n  maxsize: 7\n  maxage: 5\n  maxbackups: 1\n  usejson: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	newRootCmd()
	cfgFile = path
	t.Cleanup(func() { cfgFile = "" })

	if err := initConfig(t.Context()); err != nil {
		t.Fatalf("init config: %v", err)
	}
	want := coretypes.ServerLogConfig{Level: "warn", UseJSON: true, Filename: logPath, MaxSize: 7, MaxAge: 5, MaxBackups: 1}
	if *logConfig(t) != want {
		t.Fatalf("log config: got %+v, want %+v", *conf.Log, want)
	}
}

func TestLogMaxSizeDecodeIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cocoon.yaml")
	if err := os.WriteFile(path, []byte("log:\n  maxsize: 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	newRootCmd()
	viper.SetConfigFile(path)
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("read config: %v", err)
	}

	keys := viper.AllKeys()
	if !slices.Contains(keys, "log.maxsize") || slices.Contains(keys, "log.max_size") {
		t.Fatalf("registered log keys: got %v, want log.maxsize present and log.max_size absent", keys)
	}

	for i := range 64 {
		cfg := &config.Config{}
		if err := viper.Unmarshal(cfg); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}
		if cfg.Log.MaxSize != 7 {
			t.Fatalf("decode %d: maxsize got %d, want 7", i, cfg.Log.MaxSize)
		}
	}
}

func TestEveryConfigKeyRegistered(t *testing.T) {
	viper.Reset()
	newRootCmd()
	registered := viper.AllKeys()
	for _, key := range configKeys(reflect.TypeFor[config.Config](), "") {
		if !slices.Contains(registered, key) {
			t.Errorf("config key %s is not registered with viper, so its COCOON_* variable never reaches Unmarshal", key)
		}
	}
}

func TestCommandGroupsRejectAnUnknownVerb(t *testing.T) {
	for _, group := range [][]string{{"vm"}, {"vm", "disk"}, {"vm", "fs"}, {"vm", "device"}, {"image"}, {"snapshot"}, {"meta"}} {
		if err := executeRoot(t, group...); err != nil {
			t.Errorf("%v: bare group: %v, want its help", group, err)
		}
		if err := executeRoot(t, append(slices.Clone(group), "bogus")...); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%v bogus: err = %v, want unknown command", group, err)
		}
	}
}

func logConfig(t *testing.T) *coretypes.ServerLogConfig {
	t.Helper()
	if conf.Log == nil {
		t.Fatal("log config is nil: a lost viper default would decode as nil")
	}
	return conf.Log
}

func configKeys(typ reflect.Type, prefix string) []string {
	var keys []string
	for f := range typ.Fields() {
		name := cmp.Or(f.Tag.Get("mapstructure"), strings.ToLower(f.Name))
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			keys = append(keys, configKeys(ft, prefix+name+".")...)
			continue
		}
		keys = append(keys, prefix+name)
	}
	return keys
}

func executeRoot(t *testing.T, args ...string) error {
	t.Helper()
	viper.Reset()
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	return root.ExecuteContext(t.Context())
}
