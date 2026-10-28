//go:build !windows

package sandbox

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// within5s fails the test if fn does not return in time (a FIFO open would hang).
func within5s(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a FIFO", what)
		return nil
	}
}

func TestSendableRefusesFIFOWithoutHanging(t *testing.T) {
	p := testPolicy(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := within5s(t, "CheckSendable", func() error {
		_, _, err := p.CheckSendable(fifo)
		return err
	}); err == nil {
		t.Fatal("CheckSendable accepted a FIFO")
	}
	if err := within5s(t, "OpenSendable", func() error {
		f, _, err := p.OpenSendable(fifo)
		if f != nil {
			f.Close()
		}
		return err
	}); err == nil {
		t.Fatal("OpenSendable accepted a FIFO")
	}
	// A path swapped for a FIFO after the check must not block the open.
	within5s(t, "openNoBlock", func() error {
		f, err := openNoBlock(fifo)
		if f != nil {
			f.Close()
		}
		return err
	})
}
