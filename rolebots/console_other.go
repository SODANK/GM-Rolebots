//go:build !windows

package rolebots

// A separate console window is a Windows idea. Elsewhere the module logs to the server's own
// output.

import (
	"os"
	"syscall"
)

// newConsoleSysProcAttr returning nil tells startConsole this platform has no second window:
// it closes the pipe and logs to the main console.
func newConsoleSysProcAttr() *syscall.SysProcAttr { return nil }

// viewerOutput and prepareViewerConsole exist so the viewer compiles here too; it prints to
// whatever terminal launched it.
func viewerOutput() *os.File { return os.Stdout }

func prepareViewerConsole(out *os.File) {}
