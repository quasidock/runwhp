// Package vmm runs containers' VMs: it boots a container's guest with its
// storage and devices, supervises it as the runwhp running it, serving its
// pipe, its attached clients and its pause, and boots the helper guests
// that read image layers and stopped containers' files. The command line
// and the containerd shim both run containers through it.
package vmm

import (
	"fmt"
	"io"
	"os"
)

// Diag takes runwhp's own messages: stderr, except in a runwhp running a
// container in the background, whose stderr may be the container's, and
// in a shim, whose messages go to containerd.
var Diag io.Writer = os.Stderr

func errorf(format string, a ...any) {
	fmt.Fprintf(Diag, "runwhp: "+format+"\n", a...)
}
