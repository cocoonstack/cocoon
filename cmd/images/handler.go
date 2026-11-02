package images

import cmdcore "github.com/cocoonstack/cocoon/cmd/core"

const (
	digestDisplayLen = len("sha256:") + 12

	importSourceQcow2  importSourceKind = 0
	importSourceTar    importSourceKind = 1
	importSourceStream importSourceKind = 2
)

type Handler struct {
	cmdcore.BaseHandler
}

type importSourceKind int
