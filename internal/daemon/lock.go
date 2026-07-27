package daemon

import (
	"fmt"
	"os"
)

type Lock struct {
	file *os.File
}

func AcquireLock(paths Paths) (*Lock, error) {
	file, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure daemon lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
