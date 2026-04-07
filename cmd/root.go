package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	cmdimages "github.com/cocoonstack/cocoon/cmd/images"
	cmdothers "github.com/cocoonstack/cocoon/cmd/others"
	cmdsnapshot "github.com/cocoonstack/cocoon/cmd/snapshot"
	cmdvm "github.com/cocoonstack/cocoon/cmd/vm"
	"github.com/cocoonstack/cocoon/config"
)

var (
	cfgFile string
	conf    *config.Config

	rootCmd = func() *cobra.Command {
		cmd := &cobra.Command{
			Use:          "cocoon",
			Short:        "Cocoon - MicroVM Engine",
			SilenceUsage: true,
			PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
				return initConfig(cmdcore.CommandContext(cmd))
			},
		}

		cmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file path")
		cmd.PersistentFlags().String("root-dir", "", "root data directory")
		cmd.PersistentFlags().String("run-dir", "", "runtime directory")
		cmd.PersistentFlags().String("log-dir", "", "log directory")
		cmd.PersistentFlags().String("cni-conf-dir", "", "CNI plugin config directory (default: /etc/cni/net.d)")
		cmd.PersistentFlags().String("cni-bin-dir", "", "CNI plugin binary directory (default: /opt/cni/bin)")
		cmd.PersistentFlags().String("root-password", "", "default root password for cloudimg VMs")
		cmd.PersistentFlags().String("dns", "", `DNS servers for VMs, comma or semicolon separated (default: "8.8.8.8,1.1.1.1")`)
		cmd.PersistentFlags().String("log-level", "", `log level: debug, info, warn, error (default: "info")`)
		cmd.PersistentFlags().Bool("fc", false, "use Firecracker backend instead of Cloud Hypervisor (OCI images only)")

		_ = viper.BindPFlag("root_dir", cmd.PersistentFlags().Lookup("root-dir"))
		_ = viper.BindPFlag("run_dir", cmd.PersistentFlags().Lookup("run-dir"))
		_ = viper.BindPFlag("log_dir", cmd.PersistentFlags().Lookup("log-dir"))
		_ = viper.BindPFlag("cni_conf_dir", cmd.PersistentFlags().Lookup("cni-conf-dir"))
		_ = viper.BindPFlag("cni_bin_dir", cmd.PersistentFlags().Lookup("cni-bin-dir"))
		_ = viper.BindPFlag("default_root_password", cmd.PersistentFlags().Lookup("root-password"))
		_ = viper.BindPFlag("dns", cmd.PersistentFlags().Lookup("dns"))
		_ = viper.BindPFlag("log.level", cmd.PersistentFlags().Lookup("log-level"))
		_ = viper.BindPFlag("use_firecracker", cmd.PersistentFlags().Lookup("fc"))

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
		viper.SetDefault("log.level", "info")
		viper.SetDefault("log.max_size", 500)
		viper.SetDefault("log.max_age", 28)
		viper.SetDefault("log.max_backups", 3)

		base := cmdcore.BaseHandler{ConfProvider: func() *config.Config { return conf }}

		cmd.AddCommand(cmdimages.Command(cmdimages.Handler{BaseHandler: base}))
		cmd.AddCommand(cmdvm.Command(cmdvm.Handler{BaseHandler: base}))
		cmd.AddCommand(cmdsnapshot.Command(cmdsnapshot.Handler{BaseHandler: base}))
		for _, c := range cmdothers.Commands(cmdothers.Handler{BaseHandler: base}) {
			cmd.AddCommand(c)
		}

		return cmd
	}()
)

// Execute is the main entry point called from main.go.
func Execute() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return rootCmd.ExecuteContext(ctx)
}

func initConfig(ctx context.Context) error {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	}
	if err := viper.ReadInConfig(); err != nil {
		// No config file is OK; a corrupt/unreadable one is not.
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
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

	return log.SetupLog(ctx, conf.Log, "")
}
