// Package atomicfile replaces files without exposing partial contents.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Write publishes data at path through a mode-0600 temporary file in the same directory.
func Write(path string, data []byte) (result error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
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
