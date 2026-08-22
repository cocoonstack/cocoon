package images

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
)

func (h Handler) Export(cmd *cobra.Command, args []string) (err error) {
	ctx, conf := h.Init(cmd)
	_, cloudimgStore, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}

	ref := args[0]
	stream, err := cloudimgStore.Export(ctx, ref)
	if err != nil {
		return fmt.Errorf("export %s: %w", ref, err)
	}
	defer stream.Close() //nolint:errcheck
	defer cmdcore.CloseOnCancel(ctx, stream)()

	output, _ := cmd.Flags().GetString("output")
	if output == "-" {
		if _, err = io.Copy(os.Stdout, stream); err != nil {
			return fmt.Errorf("write cloud image: %w", err)
		}
		return nil
	}
	if output == "" {
		base := filepath.Base(ref)
		base = strings.ReplaceAll(base, ":", "-")
		output = base + ".qcow2"
	}

	log.WithFunc("cmd.images.Export").Infof(ctx, "exporting %s to %s ...", ref, output)
	return writeExportFile(output, stream)
}

// writeExportFile replaces output only after a complete, durable copy, so a
// failed export cannot truncate a previously valid image at the same path.
func writeExportFile(output string, src io.Reader) (retErr error) {
	dir := filepath.Dir(output)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(output)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create export temp file for %s: %w", output, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", output, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", output, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", output, err)
	}
	if err := os.Rename(tmpPath, output); err != nil {
		return fmt.Errorf("replace %s: %w", output, err)
	}
	return nil
}
