package main

import (
	"context"
	"sync"
	"time"

	"github.com/UserExistsError/conpty"
)

// ptyProc is the subset of a pseudo-terminal the Manager needs.
type ptyProc interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(width, height int) error
	Close() error
}

// ptyStarter starts cmdline in a new pseudo-terminal.
type ptyStarter func(cmdline, cwd string, cols, rows uint16) (ptyProc, error)

// exitDrainDelay lets the reader collect final output before the PTY closes.
const exitDrainDelay = 200 * time.Millisecond

// conPtyProc makes Close idempotent (ConPty.Close double-closes handles otherwise)
// and closes the PTY when the child exits, because ConPTY keeps the output pipe open
// after exit and Read would block forever.
type conPtyProc struct {
	*conpty.ConPty
	once     sync.Once
	cancel   context.CancelFunc
	closeErr error
}

// Close closes the pseudo-console once.
func (c *conPtyProc) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.closeErr = c.ConPty.Close()
	})
	return c.closeErr
}

// startConPty starts cmdline under ConPTY and watches for process exit.
func startConPty(cmdline, cwd string, cols, rows uint16) (ptyProc, error) {
	opts := []conpty.ConPtyOption{conpty.ConPtyDimensions(int(cols), int(rows))}
	if cwd != "" {
		opts = append(opts, conpty.ConPtyWorkDir(cwd))
	}
	cpty, err := conpty.Start(cmdline, opts...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc := &conPtyProc{ConPty: cpty, cancel: cancel}
	// Close cancels ctx before freeing handles, and proc.Close is once-guarded, so a
	// Wait that races with Close is harmless.
	go func() {
		if _, err := cpty.Wait(ctx); err == nil {
			time.Sleep(exitDrainDelay)
			proc.Close()
		}
	}()
	return proc, nil
}
