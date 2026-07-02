//go:build !windows

package conpty

import (
	"context"
	"errors"
)

var ErrUnsupported = errors.New("Windows ConPTY is only supported on Windows")

type Options struct {
	CommandLine string
	WorkDir     string
	Env         []string
	Cols        int
	Rows        int
}

type ConPty struct{}

func IsAvailable() bool { return false }

func Start(Options) (*ConPty, error)                 { return nil, ErrUnsupported }
func (*ConPty) Read([]byte) (int, error)             { return 0, ErrUnsupported }
func (*ConPty) Write([]byte) (int, error)            { return 0, ErrUnsupported }
func (*ConPty) Resize(int, int) error                { return ErrUnsupported }
func (*ConPty) Pid() uint32                          { return 0 }
func (*ConPty) Close() error                         { return nil }
func (*ConPty) Wait(context.Context) (uint32, error) { return 0, ErrUnsupported }
