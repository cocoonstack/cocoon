package version

import (
	"fmt"
	"runtime"
)

var (
	NAME     = "Cocoon"
	VERSION  = "unknown"
	REVISION = "HEAD"
	BUILTAT  = "now"
)

// String returns a multi-line version information summary.
func String() string {
	return fmt.Sprintf(
		"Version:        %s\nGit hash:       %s\nBuilt:          %s\nGolang version: %s\nOS/Arch:        %s/%s\n",
		VERSION, REVISION, BUILTAT, runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)
}
