package main

import "os"

// consolePauseMessage is shown before a double-clicked window waits for
// Enter, so the summary card or an error is not lost the instant the
// process exits and Windows closes the console with it.
const consolePauseMessage = "Press Enter to close this window."

// soleConsoleOwner reports whether processCount — the number of processes
// attached to the current console, as GetConsoleProcessList returns it on
// Windows — means this process is the console's only occupant. That only
// happens when Windows created the console for us (a double-click); a
// console inherited from an interactive cmd.exe or PowerShell session
// always holds at least the shell too.
func soleConsoleOwner(processCount int) bool {
	return processCount == 1
}

// exit runs the platform pause hook (a no-op outside Windows; see
// console_windows.go) before terminating, so a double-clicked window never
// vanishes before its summary card or error can be read.
func exit(code int) {
	pauseBeforeExit()
	os.Exit(code)
}
