package meta

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/cmd/meta/convert"
	"github.com/cocoonstack/cocoon/config"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
)

// Handler groups the meta store lifecycle commands (init, convert, backup).
type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) InitStore(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	if b := cmdcore.ResolveMetaBackend(conf); b != config.MetaBackendSQLite {
		return fmt.Errorf("meta init applies to the sqlite backend; effective meta_backend is %q", b)
	}
	if cmdcore.LegacyJSONPresent(conf) {
		return fmt.Errorf("legacy json metadata present; a fresh sqlite store would shadow it — run `cocoon meta convert`")
	}
	dbPath := cmdcore.MetaDBPath(conf)
	if err := metasqlite.Init(ctx, dbPath, cmdcore.MetaNamespaces()...); err != nil {
		return err
	}
	fmt.Printf("initialized %s\n", dbPath)
	return nil
}

func (h Handler) Convert(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	// The configured backend is the sole target authority (§6): convert
	// always moves the OTHER engine's data into the effective backend.
	target := conf.MetaBackend
	if target == "" {
		target = config.MetaBackendSQLite
	}
	dbPath := cmdcore.MetaDBPath(conf)
	spec := convert.Spec{
		MetaRoot: filepath.Dir(dbPath),
		DBPath:   dbPath,
		Decls:    cmdcore.MetaNamespaces(),
		JSON:     cmdcore.MetaJSONNamespaces(conf),
	}
	if err := convert.Run(ctx, spec, target); err != nil {
		return err
	}
	fmt.Printf("meta store converted to %s\n", target)
	return nil
}

func (h Handler) Backup(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	if b := cmdcore.ResolveMetaBackend(conf); b != config.MetaBackendSQLite {
		return fmt.Errorf("meta backup applies to the sqlite backend; effective meta_backend is %q", b)
	}
	if err := metasqlite.Backup(ctx, cmdcore.MetaDBPath(conf), args[0]); err != nil {
		return err
	}
	fmt.Printf("backed up to %s\n", args[0])
	return nil
}
