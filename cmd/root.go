package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cocoonstack/cocoon/cgroup"
	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	cmddaemon "github.com/cocoonstack/cocoon/cmd/daemon"
	cmdimages "github.com/cocoonstack/cocoon/cmd/images"
	cmdmeta "github.com/cocoonstack/cocoon/cmd/meta"
	cmdothers "github.com/cocoonstack/cocoon/cmd/others"
	cmdsnapshot "github.com/cocoonstack/cocoon/cmd/snapshot"
	cmdvm "github.com/cocoonstack/cocoon/cmd/vm"
	"github.com/cocoonstack/cocoon/config"
)

var (
	cfgFile string
	conf    *config.Config
)

func Execute(ctx context.Context) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	err := newRootCmd().ExecuteContext(ctx)
	cmdcore.CloseMetaStore(ctx)
	return err
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "cocoon",
		Short:        "Cocoon - MicroVM Engine",
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return initConfig(cliutil.CommandContext(cmd))
		},
	}

	cmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file path")
	cmd.PersistentFlags().String("root-dir", "", "root data directory")
	cmd.PersistentFlags().String("run-dir", "", "runtime directory")
	cmd.PersistentFlags().String("log-dir", "", "log directory")
	cmd.PersistentFlags().String("cni-conf-dir", "", "CNI plugin config directory (default: /etc/cni/net.d)")
	cmd.PersistentFlags().String("cni-bin-dir", "", "CNI plugin binary directory (default: /opt/cni/bin)")
	cmd.PersistentFlags().String("dns", "", `DNS servers for VMs, comma or semicolon separated (default: "8.8.8.8,1.1.1.1")`)
	cmd.PersistentFlags().String("log-level", "", `log level: debug, info, warn, error (default: "info")`)

	_ = viper.BindPFlag("root_dir", cmd.PersistentFlags().Lookup("root-dir"))
	_ = viper.BindPFlag("run_dir", cmd.PersistentFlags().Lookup("run-dir"))
	_ = viper.BindPFlag("log_dir", cmd.PersistentFlags().Lookup("log-dir"))
	_ = viper.BindPFlag("cni_conf_dir", cmd.PersistentFlags().Lookup("cni-conf-dir"))
	_ = viper.BindPFlag("cni_bin_dir", cmd.PersistentFlags().Lookup("cni-bin-dir"))
	_ = viper.BindPFlag("dns", cmd.PersistentFlags().Lookup("dns"))
	_ = viper.BindPFlag("log.level", cmd.PersistentFlags().Lookup("log-level"))

	viper.SetEnvPrefix("COCOON")
	viper.AutomaticEnv()
	viper.SetDefault("root_dir", "/var/lib/cocoon")
	viper.SetDefault("run_dir", "/var/lib/cocoon/run")
	viper.SetDefault("log_dir", "/var/log/cocoon")
	viper.SetDefault("ch_binary", "cloud-hypervisor")
	viper.SetDefault("fc_binary", "firecracker")
	viper.SetDefault("cni_conf_dir", "/etc/cni/net.d")
	viper.SetDefault("cni_bin_dir", "/opt/cni/bin")
	viper.SetDefault("dns", "8.8.8.8,1.1.1.1")
	viper.SetDefault("stop_timeout_seconds", 30)
	viper.SetDefault("pool_size", runtime.NumCPU())
	viper.SetDefault("pull_conns", 8)
	// Empty default keeps the key registered — AutomaticEnv only binds registered keys.
	viper.SetDefault("meta_backend", "")
	viper.SetDefault("cgroup_parent", cgroup.DefaultParent)
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.max_size", 500)
	viper.SetDefault("log.max_age", 28)
	viper.SetDefault("log.max_backups", 3)

	base := cmdcore.BaseHandler{ConfProvider: func() *config.Config { return conf }}

	cmd.AddCommand(cmdimages.Command(cmdimages.Handler{BaseHandler: base}))
	cmd.AddCommand(cmdvm.Command(cmdvm.Handler{BaseHandler: base}))
	cmd.AddCommand(cmdsnapshot.Command(cmdsnapshot.Handler{BaseHandler: base}))
	cmd.AddCommand(cmdmeta.Command(cmdmeta.Handler{BaseHandler: base}))
	cmd.AddCommand(cmddaemon.Command(cmddaemon.Handler{BaseHandler: base}))
	for _, c := range cmdothers.Commands(cmdothers.Handler{BaseHandler: base}) {
		cmd.AddCommand(c)
	}

	return cmd
}

func initConfig(ctx context.Context) error {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	}
	if err := viper.ReadInConfig(); err != nil {
		// No config file is OK; a corrupt/unreadable one is not.
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			return fmt.Errorf("read config: %w", err)
		}
	}

	conf = &config.Config{}
	if err := viper.Unmarshal(conf); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if err := conf.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// core/log.SetupLog captures os.Stdout; swap stderr in for the call so -o json output stays on stdout.
	origStdout := os.Stdout
	os.Stdout = os.Stderr
	setupErr := log.SetupLog(ctx, conf.Log, "")
	os.Stdout = origStdout
	return setupErr
}
