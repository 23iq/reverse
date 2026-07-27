package setup

import (
	"fmt"
	"os"
	"path/filepath"
)

type fileSnapshot struct {
	path    string
	existed bool
	data    []byte
	mode    os.FileMode
}

func atomicReplace(path string, data []byte, mode os.FileMode) (func() error, error) {
	snapshot, err := snapshotFile(path)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomically(path, data, mode); err != nil {
		return nil, err
	}
	return func() error {
		if !snapshot.existed {
			if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove new file %s: %w", snapshot.path, err)
			}
			return nil
		}
		return writeFileAtomically(snapshot.path, snapshot.data, snapshot.mode)
	}, nil
}

func snapshotFile(path string) (fileSnapshot, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return fileSnapshot{path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("refusing to replace non-regular file %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("read %s: %w", path, err)
	}
	return fileSnapshot{
		path:    path,
		existed: true,
		data:    data,
		mode:    info.Mode().Perm(),
	}, nil
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".reverse-setup-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set mode on temporary file for %s: %w", path, err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary file for %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	removeTemporary = false
	return nil
}
