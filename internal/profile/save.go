package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/atomicfile"
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
		return fmt.Errorf("validate connection to save: %w", err)
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
		return fmt.Errorf("connection %q already exists in %s; choose a different name or edit the file manually", name, path)
	}
	profiles[name] = *p
	return writeConfig(path, profiles)
}

func writeConfig(path string, profiles map[string]Profile) error {
	data, err := json.MarshalIndent(configuration{Profiles: profiles}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	if len(data) >= 1<<20 {
		return errors.New("updated configuration exceeds the 1 MiB size limit")
	}
	if err = atomicfile.Write(path, append(data, '\n')); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	return nil
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

func validateName(name string) error {
	if name == "" || !plainText(name) {
		return errors.New("connection name must be non-empty UTF-8 without control characters or surrounding whitespace")
	}
	return nil
}
