package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// UserPath returns the shared configuration location, whether or not it exists.
func UserPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user configuration directory: %w", err)
	}
	return filepath.Join(directory, "pg-tunnel", "pg-tunnel.json"), nil
}

// Save adds a validated profile without replacing an existing name. Other profiles
// are preserved; a directory lock serializes concurrent setup saves before rename.
func Save(path, name string, p *Profile) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validate profile to save: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	lock, err := os.Open(directory) //nolint:gosec // The user chose this configuration directory.
	if err != nil {
		return fmt.Errorf("open configuration directory: %w", err)
	}
	defer lock.Close() //nolint:errcheck // Closing the read-only directory releases the advisory lock.
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("lock configuration directory; retry after the other setup finishes: %w", err)
	}
	profiles, err := profilesToSave(path)
	if err != nil {
		return err
	}
	if _, exists := profiles[name]; exists {
		return fmt.Errorf("profile %q already exists in %s; choose a different name or edit the file manually", name, path)
	}
	profiles[name] = *p
	data, err := json.MarshalIndent(configuration{Profiles: profiles}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profiles: %w", err)
	}
	if len(data) >= 1<<20 {
		return errors.New("updated configuration exceeds the 1 MiB size limit")
	}
	return replaceConfig(path, append(data, '\n'))
}

func profilesToSave(path string) (map[string]Profile, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]Profile), nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("configuration must be a regular file; refusing to replace a symlink or directory")
	}
	config, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	if config.Profiles == nil {
		config.Profiles = make(map[string]Profile)
	}
	return config.Profiles, nil
}

func replaceConfig(path string, data []byte) (result error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".pg-tunnel-*")
	if err != nil {
		return fmt.Errorf("create configuration replacement: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove temporary configuration: %w", removeErr))
		}
	}()
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return fmt.Errorf("write configuration replacement: %w", err)
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	return nil
}

func validateName(name string) error {
	if name == "" || strings.ContainsAny(name, "\r\n\x00") || name != strings.TrimSpace(name) {
		return errors.New("profile name must be non-empty with no line breaks, NULs, or surrounding whitespace")
	}
	return nil
}
