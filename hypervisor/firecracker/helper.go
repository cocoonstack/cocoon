package firecracker

import "github.com/cocoonstack/cocoon/hypervisor"

const pidFileName = "fc.pid"

var runtimeFiles = []string{hypervisor.APISocketName, pidFileName, hypervisor.ConsoleSockName}
