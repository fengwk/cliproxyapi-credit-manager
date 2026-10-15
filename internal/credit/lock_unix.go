//go:build unix

package credit

import (
	"fmt"
	"os"
	"syscall"
)

// dirLock is an exclusive advisory lock over a data directory. It is held for
// the whole lifetime of a Store so a second process can neither run restart
// recovery nor account against the same database.
type dirLock struct {
	file *os.File
}

// acquireDirLock takes the lock without blocking. An already locked directory
// yields ErrLocked instead of corrupting another instance's recovery.
func acquireDirLock(path string) (*dirLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: data directory is already in use", ErrLocked)
	}
	// Keep the lock file private like the pepper.
	_ = f.Chmod(0o600)
	return &dirLock{file: f}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if cerr := l.file.Close(); err == nil {
		err = cerr
	}
	l.file = nil
	return err
}
