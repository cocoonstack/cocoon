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
	"text/tabwriter"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/types"
)

// digestDisplayLen = len("sha256:") + 12 hex digits for compact display.
const digestDisplayLen = 19

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Pull(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	ociStore, cloudimgStore, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return err
	}

	for _, image := range args {
		if cmdcore.IsURL(image) {
			if err := h.pullCloudimg(ctx, cloudimgStore, image); err != nil {
				return err
			}
		} else {
			if err := h.pullOCI(ctx, ociStore, image); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h Handler) Import(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.image.import")

	name := args[0]
	files := args[1:]

	if len(files) > 0 {
		return h.importLocalFiles(ctx, conf, name, files...)
	}

	// No file arg → stdin.
	logger.Info(ctx, "importing from stdin ...")
	return h.importFromReader(ctx, conf, name, os.Stdin)
}

func (h Handler) importLocalFiles(ctx context.Context, conf *config.Config, name string, files ...string) error {
	logger := log.WithFunc("cmd.image.import")

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

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", filePath, err)
	}
	log.WithFunc("cmd.image.import").Infof(ctx, "importing from %s ...", filePath)
	return h.importFromReader(ctx, conf, name, f)
}

// importFromReader auto-detects gzip and content type, then routes to the appropriate backend.
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
	logger := log.WithFunc("cmd.importCloudimg")
	cloudimgStore, err := cloudimg.New(ctx, conf)
	if err != nil {
		return fmt.Errorf("init cloudimg backend: %w", err)
	}
	tracker := progress.NewTracker(func(e cloudimgProgress.Event) {
		switch e.Phase {
		case cloudimgProgress.PhaseDownload:
			if len(files) == 1 {
				logger.Infof(ctx, "hashing %s", files[0])
			} else {
				logger.Infof(ctx, "hashing split qcow2 parts (%d files)", len(files))
			}
		case cloudimgProgress.PhaseConvert:
			logger.Info(ctx, "converting to qcow2...")
		case cloudimgProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case cloudimgProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", name)
		}
	})
	if err := cloudimgStore.Import(ctx, name, tracker, files...); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func (h Handler) importOCIFiles(ctx context.Context, conf *config.Config, name string, files ...string) error {
	logger := log.WithFunc("cmd.importOCI")
	ociStore, err := oci.New(ctx, conf)
	if err != nil {
		return fmt.Errorf("init oci backend: %w", err)
	}
	tracker := progress.NewTracker(func(e ociProgress.Event) {
		switch e.Phase {
		case ociProgress.PhasePull:
			logger.Infof(ctx, "importing %s (%d layer(s))", name, e.Total)
		case ociProgress.PhaseLayer:
			logger.Infof(ctx, "[%d/%d] %s done", e.Index+1, e.Total, e.Digest)
		case ociProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case ociProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", name)
		}
	})
	if err := ociStore.Import(ctx, name, tracker, files...); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func (h Handler) importCloudimgReader(ctx context.Context, conf *config.Config, name string, r io.Reader) error {
	logger := log.WithFunc("cmd.importCloudimg")
	cloudimgStore, err := cloudimg.New(ctx, conf)
	if err != nil {
		return fmt.Errorf("init cloudimg backend: %w", err)
	}
	tracker := progress.NewTracker(func(e cloudimgProgress.Event) {
		switch e.Phase {
		case cloudimgProgress.PhaseDownload:
			logger.Infof(ctx, "reading stream for %s", name)
		case cloudimgProgress.PhaseConvert:
			logger.Info(ctx, "converting to qcow2...")
		case cloudimgProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case cloudimgProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", name)
		}
	})
	if err := cloudimgStore.ImportFromReader(ctx, name, tracker, r); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func (h Handler) importOCIReader(ctx context.Context, conf *config.Config, name string, r io.Reader) error {
	logger := log.WithFunc("cmd.importOCI")
	ociStore, err := oci.New(ctx, conf)
	if err != nil {
		return fmt.Errorf("init oci backend: %w", err)
	}
	tracker := progress.NewTracker(func(e ociProgress.Event) {
		switch e.Phase {
		case ociProgress.PhasePull:
			logger.Infof(ctx, "importing %s (1 layer from stream)", name)
		case ociProgress.PhaseLayer:
			logger.Infof(ctx, "[1/1] %s done", e.Digest)
		case ociProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case ociProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", name)
		}
	})
	if err := ociStore.ImportFromReader(ctx, name, tracker, r); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	var all []*types.Image
	for _, b := range backends {
		imgs, err := b.List(ctx)
		if err != nil {
			return fmt.Errorf("list %s: %w", b.Type(), err)
		}
		all = append(all, imgs...)
	}
	if len(all) == 0 {
		fmt.Println("No images found.")
		return nil
	}

	return cmdcore.OutputFormatted(cmd, all, func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "TYPE\tNAME\tDIGEST\tSIZE\tCREATED") //nolint:errcheck
		for _, img := range all {
			digest := img.ID
			if len(digest) > digestDisplayLen {
				digest = digest[:digestDisplayLen]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
				img.Type, img.Name, digest,
				cmdcore.FormatSize(img.Size),
				img.CreatedAt.Local().Format(time.DateTime))
		}
	})
}

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.image.rm")
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	var allDeleted []string
	for _, b := range backends {
		deleted, err := b.Delete(ctx, args)
		if err != nil {
			return fmt.Errorf("delete %s: %w", b.Type(), err)
		}
		allDeleted = append(allDeleted, deleted...)
	}
	for _, ref := range allDeleted {
		logger.Infof(ctx, "deleted: %s", ref)
	}
	if len(allDeleted) == 0 {
		logger.Warn(ctx, "no matching images found")
	}
	return nil
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	ref := args[0]
	for _, b := range backends {
		img, err := b.Inspect(ctx, ref)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", b.Type(), err)
		}
		if img == nil {
			continue
		}
		return cmdcore.OutputJSON(img)
	}
	return fmt.Errorf("image %q not found", ref)
}

func (h Handler) pullOCI(ctx context.Context, store *oci.OCI, image string) error {
	logger := log.WithFunc("cmd.pullOCI")
	tracker := progress.NewTracker(func(e ociProgress.Event) {
		switch e.Phase {
		case ociProgress.PhasePull:
			logger.Infof(ctx, "pulling OCI image %s (%d layers)", image, e.Total)
		case ociProgress.PhaseLayer:
			logger.Infof(ctx, "[%d/%d] %s done", e.Index+1, e.Total, e.Digest)
		case ociProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case ociProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", image)
		}
	})
	if err := store.Pull(ctx, image, tracker); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

func (h Handler) pullCloudimg(ctx context.Context, store *cloudimg.CloudImg, url string) error {
	logger := log.WithFunc("cmd.pullCloudimg")
	tracker := progress.NewTracker(func(e cloudimgProgress.Event) {
		switch e.Phase {
		case cloudimgProgress.PhaseDownload:
			switch {
			case e.BytesDone == 0 && e.BytesTotal > 0:
				logger.Infof(ctx, "downloading cloud image %s (%s)", url, cmdcore.FormatSize(e.BytesTotal))
			case e.BytesDone == 0:
				logger.Infof(ctx, "downloading cloud image %s", url)
			case e.BytesTotal > 0:
				pct := float64(e.BytesDone) / float64(e.BytesTotal) * 100
				fmt.Printf("\r  %s / %s (%.1f%%)", cmdcore.FormatSize(e.BytesDone), cmdcore.FormatSize(e.BytesTotal), pct)
			default:
				fmt.Printf("\r  %s downloaded", cmdcore.FormatSize(e.BytesDone))
			}
		case cloudimgProgress.PhaseConvert:
			fmt.Println()
			logger.Info(ctx, "converting to qcow2...")
		case cloudimgProgress.PhaseCommit:
			logger.Info(ctx, "committing...")
		case cloudimgProgress.PhaseDone:
			logger.Infof(ctx, "done: %s", url)
		}
	})
	if err := store.Pull(ctx, url, tracker); err != nil {
		return fmt.Errorf("pull %s: %w", url, err)
	}
	return nil
}

// imageType identifies the content type detected from a stream.
type imageType int

const (
	imageTypeQcow2 imageType = iota
	imageTypeTar
)

type importSourceKind int

const (
	importSourceQcow2 importSourceKind = iota
	importSourceTar
	importSourceStream
)

type importLocalPlan struct {
	kind  importSourceKind
	files []string
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

	var magic [4]byte
	n, readErr := io.ReadFull(f, magic[:])
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("peek %s: %w", filePath, readErr)
	}

	if n >= 2 && bytes.Equal(magic[:2], []byte{0x1f, 0x8b}) {
		return importSourceStream, nil
	}
	if n >= 4 && bytes.Equal(magic[:4], []byte{'Q', 'F', 'I', 0xfb}) {
		return importSourceQcow2, nil
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek %s: %w", filePath, err)
	}
	if _, err := tar.NewReader(f).Next(); err != nil {
		return 0, fmt.Errorf("cannot detect file type for %s (expected qcow2 or tar)", filePath)
	}
	return importSourceTar, nil
}

// detectReader peeks into a reader to detect gzip wrapping and content type.
// If gzip is detected, the returned reader is unwrapped.
// The returned cleanup function must be called to release gzip resources.
func detectReader(r io.Reader) (io.Reader, imageType, func(), error) {
	br := bufio.NewReaderSize(r, 8192)

	cleanup := func() {}

	// Check for gzip magic (0x1f 0x8b).
	peek, err := br.Peek(2)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("peek: %w", err)
	}

	var inner *bufio.Reader
	if peek[0] == 0x1f && peek[1] == 0x8b {
		gr, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			return nil, 0, nil, fmt.Errorf("gzip: %w", gzErr)
		}
		cleanup = func() { _ = gr.Close() }
		inner = bufio.NewReaderSize(gr, 8192)
	} else {
		inner = br
	}

	// Check for qcow2 magic (QFI\xfb).
	cpeek, err := inner.Peek(4)
	if err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("peek content: %w", err)
	}

	if cpeek[0] == 'Q' && cpeek[1] == 'F' && cpeek[2] == 'I' && cpeek[3] == 0xfb {
		return inner, imageTypeQcow2, cleanup, nil
	}

	// Default to tar.
	return inner, imageTypeTar, cleanup, nil
}
