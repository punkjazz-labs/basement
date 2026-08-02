//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetConsoleProcessList has no Go binding in golang.org/x/sys/windows, so it
// is bound directly the same way that package binds every other kernel32
// call it does wrap.
var (
	modKernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = modKernel32.NewProc("GetConsoleProcessList")
)

// consoleProcessCount returns how many processes are attached to this
// process's console. A one-element buffer is enough: per the Win32 docs, if
// the buffer is too small to hold every process ID, the call still returns
// the true total count rather than failing, and soleConsoleOwner only ever
// needs to tell "just us" apart from "more than us".
func consoleProcessCount() int {
	var pid [1]uint32
	r1, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pid[0])), 1)
	return int(r1)
}

// pauseBeforeExit keeps a double-clicked window open long enough to read the
// summary card or an error. It only fires when this process is the console's
// sole owner (see soleConsoleOwner) — never when launched from an existing
// cmd.exe or PowerShell session, which already shares the console with the
// shell.
func pauseBeforeExit() {
	if !soleConsoleOwner(consoleProcessCount()) {
		return
	}
	fmt.Println(consolePauseMessage)
	bufio.NewReader(os.Stdin).ReadString('\n')
}
