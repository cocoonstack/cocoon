package images

import cmdcore "github.com/cocoonstack/cocoon/cmd/core"

type Handler struct {
	cmdcore.BaseHandler
}

// imageType identifies the content type detected from a stream.
type imageType int

type importSourceKind int

const (
	// digestDisplayLen = len("sha256:") + 12 hex digits for compact display.
	digestDisplayLen = 19

	imageTypeQcow2 imageType = 0
	imageTypeTar   imageType = 1

	importSourceQcow2  importSourceKind = 0
	importSourceTar    importSourceKind = 1
	importSourceStream importSourceKind = 2
)

type importLocalPlan struct {
	kind  importSourceKind
	files []string
}
