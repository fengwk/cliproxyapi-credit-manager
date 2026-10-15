//go:build !unix

package credit

import (
	"fmt"
	"runtime"
)

// dirLock is a no-op placeholder on platforms without flock support. The
// released targets are linux and darwin.
type dirLock struct{}

func acquireDirLock(string) (*dirLock, error) {
	return nil, fmt.Errorf("%w: file locking is unsupported on %s", ErrLocked, runtime.GOOS)
}

func (l *dirLock) release() error { return nil }
