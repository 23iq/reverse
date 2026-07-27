package setup

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	reverseassets "github.com/23iq/reverse"
)

const embeddedBuildContextPrefix = "reverse-server-build-"

func prepareBuildContext(sourceDir string) (string, func(), error) {
	if sourceDir != "" {
		if err := validateBuildContext(sourceDir); err != nil {
			return "", nil, err
		}
		return sourceDir, func() {}, nil
	}

	// Setup runs as root. Do not trust an inherited TMPDIR in that case: its
	// owner could rename the materialized context before Docker opens it. /run
	// is a root-owned systemd runtime directory on supported VPS hosts.
	tempRoot := buildContextTempRoot(os.Geteuid())
	directory, err := os.MkdirTemp(tempRoot, embeddedBuildContextPrefix)
	if err != nil {
		return "", nil, fmt.Errorf("create embedded server build context: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(directory)
	}
	if err := materializeBuildContext(directory); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := validateBuildContext(directory); err != nil {
		cleanup()
		return "", nil, err
	}
	return directory, cleanup, nil
}

func buildContextTempRoot(effectiveUID int) string {
	if effectiveUID == 0 {
		return "/run"
	}
	return ""
}

func materializeBuildContext(destination string) error {
	return fs.WalkDir(reverseassets.ServerBuildContext, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("read embedded server build context %s: %w", name, walkErr)
		}
		if name == "." {
			return nil
		}
		if !fs.ValidPath(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("embedded server build context contains unsafe path %q", name)
		}

		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create embedded server build directory %s: %w", target, err)
			}
			return nil
		}
		if entry.Type()&fs.ModeType != 0 {
			return fmt.Errorf("embedded server build context contains non-regular file %q", name)
		}
		data, err := fs.ReadFile(reverseassets.ServerBuildContext, name)
		if err != nil {
			return fmt.Errorf("read embedded server build file %s: %w", name, err)
		}
		if err := writeFileAtomically(target, data, 0o644); err != nil {
			return fmt.Errorf("materialize embedded server build file %s: %w", name, err)
		}
		return nil
	})
}

func validateBuildContext(directory string) error {
	info, err := os.Stat(filepath.Join(directory, "Dockerfile"))
	if err != nil {
		return fmt.Errorf("source directory must contain Dockerfile: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source directory must contain Dockerfile: path is not a regular file")
	}
	return nil
}
