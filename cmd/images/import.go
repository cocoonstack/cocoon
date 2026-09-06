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

	plan, err := planLocalImport(files)
	if err != nil {
		return err
	}

	switch plan.kind {
	case importSourceQcow2:
		if len(plan.files) == 1 {
			logger.Infof(ctx, "importing qcow2 file %s ...", plan.files[0])
		} else {
			logger.Infof(ctx, "importing split qcow2 parts (%d files) ...", len(plan.files))
		}
		return h.importCloudimgFiles(ctx, conf, name, plan.files...)
	case importSourceTar:
		if len(plan.files) == 1 {
			logger.Infof(ctx, "importing tar file %s ...", plan.files[0])
		} else {
			logger.Infof(ctx, "importing tar layers (%d files) ...", len(plan.files))
		}
		return h.importOCIFiles(ctx, conf, name, plan.files...)
	case importSourceStream:
		return h.importLocalStream(ctx, conf, name, plan.files[0])
	default:
		return fmt.Errorf("unsupported local import source")
	}
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

	switch typ {
	case imageTypeQcow2:
		return h.importCloudimgReader(ctx, conf, name, reader)
	case imageTypeTar:
		return h.importOCIReader(ctx, conf, name, reader)
	default:
		return fmt.Errorf("unsupported image type")
	}
}

func (h Handler) importCloudimgFiles(ctx context.Context, conf *config.Config, name string, files ...string) error {
	return importCloudimg(ctx, conf, name, func() string {
		if len(files) == 1 {
			return "hashing " + files[0]
		}
		return fmt.Sprintf("hashing split qcow2 parts (%d files)", len(files))
	}, func(store *cloudimg.CloudImg, tracker progress.Tracker) error {
		return store.Import(ctx, name, tracker, files...)
	})
}

func (h Handler) importOCIFiles(ctx context.Context, conf *config.Config, name string, files ...string) error {
	return importOCI(ctx, conf, name, func(total int) string {
		return fmt.Sprintf("importing %s (%d layer(s))", name, total)
	}, func(store *oci.OCI, tracker progress.Tracker) error {
		return store.Import(ctx, name, tracker, files...)
	})
}

func (h Handler) importCloudimgReader(ctx context.Context, conf *config.Config, name string, r io.Reader) error {
	return importCloudimg(ctx, conf, name, func() string { return "reading stream for " + name }, func(store *cloudimg.CloudImg, tracker progress.Tracker) error {
		return store.ImportFromReader(ctx, name, tracker, r)
	})
}

func (h Handler) importOCIReader(ctx context.Context, conf *config.Config, name string, r io.Reader) error {
	return importOCI(ctx, conf, name, func(int) string { return fmt.Sprintf("importing %s (1 layer from stream)", name) }, func(store *oci.OCI, tracker progress.Tracker) error {
		return store.ImportFromReader(ctx, name, tracker, r)
	})
}

func importCloudimg(ctx context.Context, conf *config.Config, name string, downloadMsg func() string, do func(*cloudimg.CloudImg, progress.Tracker) error) error {
	_, store, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}
	if err := do(store, cloudimgImportTracker(ctx, log.WithFunc("cmd.images.importCloudimg"), name, downloadMsg)); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func importOCI(ctx context.Context, conf *config.Config, name string, pullMsg func(int) string, do func(*oci.OCI, progress.Tracker) error) error {
	store, _, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}
	if err := do(store, ociTracker(ctx, log.WithFunc("cmd.images.importOCI"), name, pullMsg)); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func planLocalImport(files []string) (importLocalPlan, error) {
	if len(files) == 0 {
		return importLocalPlan{}, fmt.Errorf("no local files provided")
	}
	kind, err := detectLocalImportSource(files[0])
	if err != nil {
		return importLocalPlan{}, err
	}
	if kind == importSourceStream && len(files) > 1 {
		return importLocalPlan{}, fmt.Errorf("stream imports accept exactly one file, got %d", len(files))
	}
	return importLocalPlan{kind: kind, files: files}, nil
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

func detectReader(r io.Reader) (io.Reader, imageType, func(), error) {
	br := bufio.NewReaderSize(r, 8192)

	cleanup := func() {}

	peek, err := br.Peek(2)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("peek: %w", err)
	}

	var inner *bufio.Reader
	if bytes.HasPrefix(peek, utils.GzipMagic) {
		gr, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			return nil, 0, nil, fmt.Errorf("gzip: %w", gzErr)
		}
		cleanup = func() { _ = gr.Close() }
		inner = bufio.NewReaderSize(gr, 8192)
	} else {
		inner = br
	}

	cpeek, err := inner.Peek(4)
	if err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("peek content: %w", err)
	}

	if bytes.HasPrefix(cpeek, utils.Qcow2Magic) {
		return inner, imageTypeQcow2, cleanup, nil
	}

	return inner, imageTypeTar, cleanup, nil
}
