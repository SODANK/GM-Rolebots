//go:build windows

package rolebots

// The Windows half of the module's log window: asking for a console when the viewer is
// spawned, and making that console behave once it is up.

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	createNewConsole = 0x00000010

	enableProcessedOutput           = 0x0001
	enableWrapAtEOL                 = 0x0002
	enableVirtualTerminalProcessing = 0x0004

	enableQuickEdit    = 0x0040
	enableExtendedFlag = 0x0080

	genericRead  = 0x80000000
	genericWrite = 0x40000000
	fileShareRW  = 0x00000003
	openExisting = 3
)

// newConsoleSysProcAttr gives the child a console of its own. Without the flag the child
// inherits the server's console.
func newConsoleSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewConsole}
}

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode  = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode  = kernel32.NewProc("SetConsoleMode")
	procSetConsoleTitle = kernel32.NewProc("SetConsoleTitleW")
)

// viewerOutput is the window the viewer paints into. It is not os.Stdout: the viewer is
// spawned with Stdout nil, which os/exec turns into the NUL device, so the window would open
// and stay blank. The console is opened by name (CONOUT$). os.Stdout is the fallback for a
// viewer run by hand from a terminal.
func viewerOutput() *os.File {
	h, err := syscall.CreateFile(utf16Ptr("CONOUT$"), genericRead|genericWrite,
		fileShareRW, nil, openExisting, 0, 0)
	if err != nil {
		return os.Stdout
	}
	return os.NewFile(uintptr(h), "CONOUT$")
}

// prepareViewerConsole runs in the viewer process. Everything here is best-effort: a console
// that refuses a call still shows the log, with escape codes visible or QuickEdit on.
func prepareViewerConsole(out *os.File) {
	setTitle("RoleBots — activity log")

	// Colour: Windows 10 renders ANSI only once this bit is set on the console output handle the
	// viewer writes to, not STD_OUTPUT_HANDLE (the NUL device here, which accepts it and does nothing).
	if out != nil {
		h := syscall.Handle(out.Fd())
		var mode uint32
		if getConsoleMode(h, &mode) {
			setConsoleMode(h, mode|enableProcessedOutput|enableWrapAtEOL|enableVirtualTerminalProcessing)
		}
	}

	// QuickEdit off: a stray click would select text and freeze console output until Escape. The
	// viewer's stdin is the server's pipe, so the console input buffer is opened by name.
	conin := utf16Ptr("CONIN$")
	h, err := syscall.CreateFile(conin, genericRead|genericWrite, fileShareRW,
		nil, openExisting, 0, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)
	var mode uint32
	if getConsoleMode(h, &mode) {
		setConsoleMode(h, (mode&^enableQuickEdit)|enableExtendedFlag)
	}
}

func getConsoleMode(h syscall.Handle, mode *uint32) bool {
	r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(mode)))
	return r != 0
}

func setConsoleMode(h syscall.Handle, mode uint32) {
	procSetConsoleMode.Call(uintptr(h), uintptr(mode))
}

func setTitle(s string) {
	procSetConsoleTitle.Call(uintptr(unsafe.Pointer(utf16Ptr(s))))
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}
