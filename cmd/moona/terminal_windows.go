//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	enableProcessedInput        uint32 = 0x0001
	enableLineInput             uint32 = 0x0002
	enableEchoInput             uint32 = 0x0004
	enableVirtualTerminalInput  uint32 = 0x0200
	enableVirtualTerminalOutput uint32 = 0x0004
	disableNewlineAutoReturn    uint32 = 0x0008
)

type consoleScreenBufferInfo struct {
	Size              coordXY
	CursorPosition    coordXY
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coordXY
}

type coordXY struct {
	X int16
	Y int16
}

type smallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

var (
	consoleKernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleScreenBufferInfo = consoleKernel32.NewProc("GetConsoleScreenBufferInfo")
)

func prepareLocalTerminal() (func(), int, int, error) {
	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return nil, 0, 0, err
	}
	out, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, 0, 0, err
	}

	var originalIn uint32
	if err := windows.GetConsoleMode(in, &originalIn); err != nil {
		return nil, 0, 0, fmt.Errorf("get input console mode: %w", err)
	}
	var originalOut uint32
	if err := windows.GetConsoleMode(out, &originalOut); err != nil {
		return nil, 0, 0, fmt.Errorf("get output console mode: %w", err)
	}

	rawIn := originalIn
	rawIn &^= enableProcessedInput | enableLineInput | enableEchoInput
	rawIn |= enableVirtualTerminalInput
	if err := windows.SetConsoleMode(in, rawIn); err != nil {
		return nil, 0, 0, fmt.Errorf("set input raw mode: %w", err)
	}

	rawOut := originalOut | enableVirtualTerminalOutput | disableNewlineAutoReturn
	if err := windows.SetConsoleMode(out, rawOut); err != nil {
		_ = windows.SetConsoleMode(in, originalIn)
		return nil, 0, 0, fmt.Errorf("set output virtual terminal mode: %w", err)
	}

	cols, rows := localConsoleSize(out)
	restore := func() {
		_ = windows.SetConsoleMode(in, originalIn)
		_ = windows.SetConsoleMode(out, originalOut)
	}
	return restore, cols, rows, nil
}

// currentConsoleSize reports the live size of this process's output console,
// or 0,0 if unavailable. Used to poll for native-terminal window resizes.
func currentConsoleSize() (int, int) {
	out, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return 0, 0
	}
	return localConsoleSize(out)
}

func localConsoleSize(out windows.Handle) (int, int) {
	if procGetConsoleScreenBufferInfo.Find() != nil {
		return 0, 0
	}
	var info consoleScreenBufferInfo
	ret, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(out), uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return 0, 0
	}
	cols := int(info.Window.Right - info.Window.Left + 1)
	rows := int(info.Window.Bottom - info.Window.Top + 1)
	if cols <= 0 || rows <= 0 {
		return 0, 0
	}
	return cols, rows
}

func prepareShortcutConfirmationInput() (func(), error) {
	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return func() {}, err
	}

	var originalIn uint32
	if err := windows.GetConsoleMode(in, &originalIn); err != nil {
		return func() {}, err
	}

	cookedIn := originalIn
	cookedIn |= enableProcessedInput | enableLineInput | enableEchoInput
	cookedIn &^= enableVirtualTerminalInput
	if err := windows.SetConsoleMode(in, cookedIn); err != nil {
		return func() {}, err
	}

	return func() {
		_ = windows.SetConsoleMode(in, originalIn)
	}, nil
}
