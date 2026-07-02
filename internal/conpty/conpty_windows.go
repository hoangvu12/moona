//go:build windows

package conpty

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procCreatePseudoConsole               = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole               = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole                = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
)

var ErrUnsupported = errors.New("Windows ConPTY is not available; Windows 10 1809 or newer is required")

const (
	sOK                              uintptr = 0
	stillActive                      uint32  = 259
	procThreadAttributePseudoConsole uintptr = 0x00020016
	defaultConsoleWidth                      = 100
	defaultConsoleHeight                     = 30
)

type coord struct {
	X int16
	Y int16
}

func (c coord) pack() uintptr {
	return uintptr(uint32(uint16(c.X)) | uint32(uint16(c.Y))<<16)
}

type hpcon windows.Handle

type handleIO struct {
	handle windows.Handle
}

func (h handleIO) Read(p []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(h.handle, p, &n, nil)
	return int(n), err
}

func (h handleIO) Write(p []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(h.handle, p, &n, nil)
	return int(n), err
}

func (h handleIO) Close() error {
	if h.handle == 0 || h.handle == windows.InvalidHandle {
		return nil
	}
	return windows.CloseHandle(h.handle)
}

// ConPty is a small io.ReadWriter wrapper around a Windows pseudoconsole.
type ConPty struct {
	hpc          hpcon
	pi           windows.ProcessInformation
	inputWriter  handleIO
	outputReader handleIO

	// Kept so they can be closed when the pseudoconsole shuts down.
	ptyInput  windows.Handle
	ptyOutput windows.Handle
}

// Options configure a ConPTY process.
type Options struct {
	CommandLine string
	WorkDir     string
	Env         []string
	Cols        int
	Rows        int
}

func IsAvailable() bool {
	return procCreatePseudoConsole.Find() == nil &&
		procResizePseudoConsole.Find() == nil &&
		procClosePseudoConsole.Find() == nil &&
		procInitializeProcThreadAttributeList.Find() == nil &&
		procUpdateProcThreadAttribute.Find() == nil
}

func Start(opts Options) (*ConPty, error) {
	if !IsAvailable() {
		return nil, ErrUnsupported
	}
	if opts.CommandLine == "" {
		return nil, errors.New("command line is required")
	}
	if opts.Cols <= 0 {
		opts.Cols = defaultConsoleWidth
	}
	if opts.Rows <= 0 {
		opts.Rows = defaultConsoleHeight
	}

	var ptyIn, cmdIn, cmdOut, ptyOut windows.Handle
	if err := windows.CreatePipe(&ptyIn, &cmdIn, nil, 0); err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	if err := windows.CreatePipe(&cmdOut, &ptyOut, nil, 0); err != nil {
		closeHandles(ptyIn, cmdIn)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}

	hpc, err := createPseudoConsole(coord{X: int16(opts.Cols), Y: int16(opts.Rows)}, ptyIn, ptyOut)
	if err != nil {
		closeHandles(ptyIn, cmdIn, cmdOut, ptyOut)
		return nil, err
	}

	pi, err := createProcessAttachedToPseudoConsole(hpc, opts.CommandLine, opts.WorkDir, opts.Env)
	if err != nil {
		closePseudoConsole(hpc)
		closeHandles(ptyIn, cmdIn, cmdOut, ptyOut)
		return nil, err
	}

	return &ConPty{
		hpc:          hpc,
		pi:           pi,
		inputWriter:  handleIO{handle: cmdIn},
		outputReader: handleIO{handle: cmdOut},
		ptyInput:     ptyIn,
		ptyOutput:    ptyOut,
	}, nil
}

func (c *ConPty) Read(p []byte) (int, error) {
	return c.outputReader.Read(p)
}

func (c *ConPty) Write(p []byte) (int, error) {
	return c.inputWriter.Write(p)
}

func (c *ConPty) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return resizePseudoConsole(c.hpc, coord{X: int16(cols), Y: int16(rows)})
}

func (c *ConPty) Pid() uint32 {
	return c.pi.ProcessId
}

func (c *ConPty) Close() error {
	closePseudoConsole(c.hpc)
	return closeHandles(
		c.pi.Process,
		c.pi.Thread,
		c.inputWriter.handle,
		c.outputReader.handle,
		c.ptyInput,
		c.ptyOutput,
	)
}

func (c *ConPty) Wait(ctx context.Context) (uint32, error) {
	for {
		if err := ctx.Err(); err != nil {
			return stillActive, err
		}
		status, err := windows.WaitForSingleObject(c.pi.Process, 250)
		if err != nil {
			return stillActive, err
		}
		if status == 0x00000102 { // WAIT_TIMEOUT
			continue
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(c.pi.Process, &exitCode); err != nil {
			return stillActive, err
		}
		return exitCode, nil
	}
}

func createPseudoConsole(size coord, hInput, hOutput windows.Handle) (hpcon, error) {
	var hpc hpcon
	ret, _, err := procCreatePseudoConsole.Call(
		size.pack(),
		uintptr(hInput),
		uintptr(hOutput),
		0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	if ret != sOK {
		return 0, fmt.Errorf("CreatePseudoConsole failed: HRESULT 0x%x (%v)", ret, err)
	}
	return hpc, nil
}

func resizePseudoConsole(hpc hpcon, size coord) error {
	ret, _, err := procResizePseudoConsole.Call(uintptr(hpc), size.pack())
	if ret != sOK {
		return fmt.Errorf("ResizePseudoConsole failed: HRESULT 0x%x (%v)", ret, err)
	}
	return nil
}

func closePseudoConsole(hpc hpcon) {
	if hpc != 0 {
		procClosePseudoConsole.Call(uintptr(hpc))
	}
}

func createProcessAttachedToPseudoConsole(hpc hpcon, commandLine, workDir string, _ []string) (windows.ProcessInformation, error) {
	cmdLine, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("encode command line: %w", err)
	}

	attributeList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("create process attribute list: %w", err)
	}
	defer attributeList.Delete()

	if err := attributeList.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(hpc),
		unsafe.Sizeof(hpc),
	); err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("set ConPTY process attribute: %w", err)
	}

	siEx := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attributeList.List(),
	}

	var cwd *uint16
	if workDir != "" {
		cwd, err = windows.UTF16PtrFromString(workDir)
		if err != nil {
			return windows.ProcessInformation{}, fmt.Errorf("encode working directory: %w", err)
		}
	}

	var envBlock *uint16

	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		nil,
		cmdLine,
		nil,
		nil,
		false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		envBlock,
		cwd,
		&siEx.StartupInfo,
		&pi,
	)
	if err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("CreateProcess failed: %w", err)
	}
	return pi, nil
}

func closeHandles(handles ...windows.Handle) error {
	var first error
	for _, h := range handles {
		if h == 0 || h == windows.InvalidHandle {
			continue
		}
		if err := windows.CloseHandle(h); err != nil && first == nil {
			first = err
		}
	}
	return first
}
