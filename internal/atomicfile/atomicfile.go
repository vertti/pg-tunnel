// Package atomicfile replaces files without exposing partial contents.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Write publishes data at path through a mode-0600 temporary file in the same directory.
func Write(path string, data []byte) (result error) {
	file, err := os.CreateTemp(filepath.Dir(path), tempPattern(path))
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove temporary file: %w", removeErr))
		}
	}()
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// RemoveLeftovers deletes temporary files that a killed Write left next to path.
// Callers must exclude concurrent writers of path.
func RemoveLeftovers(path string) error {
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), tempPattern(path)))
	if err != nil {
		return fmt.Errorf("find temporary files: %w", err)
	}
	for _, leftover := range leftovers {
		if err = os.Remove(leftover); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove temporary file: %w", err)
		}
	}
	return nil
}

func tempPattern(path string) string {
	return "." + strings.TrimPrefix(filepath.Base(path), ".") + ".tmp-*"
}
