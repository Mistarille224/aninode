package filesystem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrWriterLocked = errors.New("another aninode writer is active")

// WriterLock is a kernel-owned advisory lock. The lock file is only a rendezvous
// point; ownership lives in the open file description and disappears when the
// process/file descriptor exits, so it is not persistent workflow state.
type WriterLock struct {
	file *os.File
}

func AcquireWriterLock(root string) (*WriterLock, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	path := filepath.Join(root, ".aninode.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open writer lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrWriterLocked
		}
		return nil, fmt.Errorf("acquire writer lock: %w", err)
	}
	return &WriterLock{file: f}, nil
}

func (l *WriterLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
