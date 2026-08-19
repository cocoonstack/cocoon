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

	f, err := os.Create(output) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create %s: %w", output, err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(output)
		}
	}()

	log.WithFunc("cmd.images.Export").Infof(ctx, "exporting %s to %s ...", ref, output)
	if _, err = io.Copy(f, stream); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	return nil
}
