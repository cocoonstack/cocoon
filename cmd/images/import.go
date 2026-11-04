package images

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/utils"
)

func (h Handler) Import(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
	logger := log.WithFunc("cmd.images.Import")

	name := args[0]
	files := args[1:]

	if len(files) > 0 {
		return h.importLocalFiles(ctx, conf, name, files...)
	}

	logger.Info(ctx, "importing from stdin ...")
	return h.importFromReader(ctx, conf, name, os.Stdin)
}

func (h Handler) importLocalFiles(ctx context.Context, conf *config.Config, name string, files ...string) error {
	logger := log.WithFunc("cmd.images.importLocalFiles")

	kind, err := planLocalImport(files)
	if err != nil {
		return err
	}

	switch kind {
	case importSourceQcow2:
		if len(files) == 1 {
			logger.Infof(ctx, "importing qcow2 file %s ...", files[0])
		} else {
			logger.Infof(ctx, "importing split qcow2 parts (%d files) ...", len(files))
		}
		msg := "hashing " + files[0]
		if len(files) > 1 {
			msg = fmt.Sprintf("hashing split qcow2 parts (%d files)", len(files))
		}
		return importCloudimg(ctx, conf, name, msg, func(store *cloudimg.CloudImg, tracker progress.Tracker) error {
			return store.Import(ctx, name, tracker, files...)
		})
	case importSourceTar:
		if len(files) == 1 {
			logger.Infof(ctx, "importing tar file %s ...", files[0])
		} else {
			logger.Infof(ctx, "importing tar layers (%d files) ...", len(files))
		}
		pullMsg := func(total int) string { return fmt.Sprintf("importing %s (%d layer(s))", name, total) }
		return importOCI(ctx, conf, name, pullMsg, func(store *oci.OCI, tracker progress.Tracker) error {
			return store.Import(ctx, name, tracker, files...)
		})
	}
	return h.importLocalStream(ctx, conf, name, files[0])
}

func (h Handler) importLocalStream(ctx context.Context, conf *config.Config, name, filePath string) error {
	f, openErr := os.Open(filePath) //nolint:gosec
	if openErr != nil {
		return fmt.Errorf("open %s: %w", filePath, openErr)
	}
	defer f.Close() //nolint:errcheck

	logger := log.WithFunc("cmd.images.importLocalStream")
	logger.Infof(ctx, "importing from %s ...", filePath)
	return h.importFromReader(ctx, conf, name, f)
}

func (h Handler) importFromReader(ctx context.Context, conf *config.Config, name string, r io.Reader) error {
	reader, typ, cleanup, err := detectReader(r)
	if err != nil {
		return fmt.Errorf("detect image type: %w", err)
	}
	defer cleanup()

	if typ == importSourceQcow2 {
		return importCloudimg(ctx, conf, name, "reading stream for "+name, func(store *cloudimg.CloudImg, tracker progress.Tracker) error {
			return store.ImportFromReader(ctx, name, tracker, reader)
		})
	}
	pullMsg := func(int) string { return fmt.Sprintf("importing %s (1 layer from stream)", name) }
	return importOCI(ctx, conf, name, pullMsg, func(store *oci.OCI, tracker progress.Tracker) error {
		return store.ImportFromReader(ctx, name, tracker, reader)
	})
}

func importCloudimg(ctx context.Context, conf *config.Config, name, downloadMsg string, do func(*cloudimg.CloudImg, progress.Tracker) error) error {
	_, store, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}
	if err := do(store, cloudimgImportTracker(ctx, log.WithFunc("cmd.images.importCloudimg"), name, downloadMsg)); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func importOCI(ctx context.Context, conf *config.Config, name string, pullMsg pullMsgFunc, do func(*oci.OCI, progress.Tracker) error) error {
	store, _, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}
	if err := do(store, ociTracker(ctx, log.WithFunc("cmd.images.importOCI"), name, pullMsg)); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func planLocalImport(files []string) (importSourceKind, error) {
	kind, err := detectLocalImportSource(files[0])
	if err != nil {
		return 0, err
	}
	if kind == importSourceStream && len(files) > 1 {
		return 0, fmt.Errorf("stream imports accept exactly one file, got %d", len(files))
	}
	return kind, nil
}

func detectLocalImportSource(filePath string) (importSourceKind, error) {
	f, err := os.Open(filePath) //nolint:gosec
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", filePath, err)
	}
	defer func() { _ = f.Close() }()

	head, err := utils.FileHead(f, 4)
	if err != nil {
		return 0, fmt.Errorf("peek %s: %w", filePath, err)
	}
	if bytes.HasPrefix(head, utils.GzipMagic) {
		return importSourceStream, nil
	}
	if bytes.HasPrefix(head, utils.Qcow2Magic) {
		return importSourceQcow2, nil
	}

	if _, err := tar.NewReader(f).Next(); err != nil {
		return 0, fmt.Errorf("cannot detect file type for %s (expected qcow2 or tar)", filePath)
	}
	return importSourceTar, nil
}

func detectReader(r io.Reader) (io.Reader, importSourceKind, func(), error) {
	br := bufio.NewReaderSize(r, 8192)

	cleanup := func() {}

	peek, err := br.Peek(2)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("peek: %w", err)
	}

	inner := br
	if bytes.HasPrefix(peek, utils.GzipMagic) {
		gr, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			return nil, 0, nil, fmt.Errorf("gzip: %w", gzErr)
		}
		cleanup = func() { _ = gr.Close() }
		inner = bufio.NewReaderSize(gr, 8192)
	}

	cpeek, err := inner.Peek(4)
	if err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("peek content: %w", err)
	}

	if bytes.HasPrefix(cpeek, utils.Qcow2Magic) {
		return inner, importSourceQcow2, cleanup, nil
	}

	return inner, importSourceTar, cleanup, nil
}
