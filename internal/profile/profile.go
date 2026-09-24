// Package profile reads non-secret project and user connection profiles.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// AuthSecretsManager selects password authentication from an explicitly chosen secret.
const AuthSecretsManager = "secrets-manager"

// Environment classifications; production prints a warning at startup.
const (
	EnvironmentDevelopment = "development"
	EnvironmentStaging     = "staging"
	EnvironmentProduction  = "production"
)

// Profile configures a database session without storing credentials.
type Profile struct {
	Environment string `json:"environment,omitempty"`
	Auth        string `json:"auth,omitempty"`
	SecretID    string `json:"secret_id,omitempty"`
	DBInstance  string `json:"db_instance"`
	Host        string `json:"host"`
	Database    string `json:"database"`
	User        string `json:"user"`
	Target      string `json:"target"`
	JumpTag     string `json:"jump_tag"`
	Region      string `json:"region"`
	AWSProfile  string `json:"aws_profile"`
	RootCert    string `json:"sslrootcert,omitempty"`
	Port        int    `json:"port"`
	LocalPort   int    `json:"local_port"`
	// Project marks a profile read implicitly from the current directory.
	Project bool `json:"-"`
}

// Load reads one named profile; certificate paths are relative to its file.
// An empty path searches the current directory, then the user config directory.
func Load(path, name string) (Profile, error) {
	path, project, err := configPath(path)
	if err != nil {
		return Profile{}, err
	}
	value, err := load(path, name)
	value.Project = project
	return value, err
}

func configPath(path string) (_ string, project bool, _ error) {
	if path != "" {
		return path, false, nil
	}
	const filename = "pg-tunnel.json"
	// A broken symlink or unreadable project config must not select another database.
	if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
		return filename, true, nil
	}
	path, err := UserPath()
	if err != nil {
		return "", false, fmt.Errorf("no project %s; find user configuration directory (or use --config PATH): %w", filename, err)
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("no configuration file found: checked %s in the current directory and %s; create one or use --config PATH", filename, path)
	}
	return path, false, nil
}

type configuration struct {
	Profiles map[string]Profile `json:"profiles"`
}

func readConfig(path string) (configuration, error) {
	file, err := os.Open(path) //nolint:gosec // The path is an explicit configuration file or a documented default location.
	if err != nil {
		return configuration{}, fmt.Errorf("open profiles %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // This file is read-only.
	var config configuration
	limited := &io.LimitedReader{R: file, N: (1 << 20) + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&config)
	if err == nil && !errors.Is(decoder.Decode(new(any)), io.EOF) {
		err = errors.New("must contain exactly one JSON object")
	}
	if limited.N == 0 {
		return configuration{}, errors.New("configuration exceeds the 1 MiB size limit")
	}
	if err != nil {
		return configuration{}, fmt.Errorf("decode profiles %s: %w", path, err)
	}
	return config, nil
}

func load(path, name string) (Profile, error) {
	config, err := readConfig(path)
	if err != nil {
		return Profile{}, err
	}
	value, ok := config.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("profile %q does not exist in %s", name, path)
	}
	if value.Port == 0 {
		value.Port = 5432
	}
	if err = value.Validate(); err != nil {
		return Profile{}, fmt.Errorf("profile %q in %s: %w", name, path, err)
	}
	if value.RootCert == "" {
		return value, nil
	}
	if !filepath.IsAbs(value.RootCert) {
		value.RootCert = filepath.Join(filepath.Dir(path), value.RootCert)
	}
	value.RootCert, err = filepath.Abs(value.RootCert)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve CA certificate path: %w", err)
	}
	return value, nil
}

// Validate rejects ambiguous discovery and unsafe client-file values.
func (p *Profile) Validate() error {
	if (p.DBInstance == "") == (p.Host == "") {
		return errors.New("set exactly one of db_instance or host")
	}
	if (p.Target == "") == (p.JumpTag == "") {
		return errors.New("set exactly one of target (SSM instance ID) or jump_tag (EC2 Name tag)")
	}
	if p.Database == "" || p.User == "" {
		return errors.New("database and user are required")
	}
	if p.LocalPort < 0 || p.LocalPort > 65535 || p.Port < 1 || p.Port > 65535 {
		return errors.New("port must be 1–65535; local_port must be 0–65535 (0 selects an available port)")
	}
	if err := p.validateOptions(); err != nil {
		return err
	}
	return p.validateText()
}

func (p *Profile) validateText() error {
	if p.Host != "" && p.RootCert == "" {
		return errors.New("sslrootcert is required for an explicit host; RDS instance profiles can use automatic certificates")
	}

	for _, value := range []string{p.DBInstance, p.Host, p.Database, p.User, p.Target, p.JumpTag, p.Region, p.AWSProfile, p.RootCert, p.SecretID} {
		if !plainText(value) {
			return errors.New("profile values cannot contain line breaks, NULs, or surrounding whitespace")
		}
	}
	if strings.ContainsAny(p.Host, "/:, \\*") || strings.ContainsAny(p.Database, "*") || strings.ContainsAny(p.User, "*") {
		return errors.New("host must be a single DNS name; database and user cannot contain password-file wildcards")
	}
	return nil
}

func plainText(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00") && value == strings.TrimSpace(value)
}

func (p *Profile) validateOptions() error {
	switch p.Environment {
	case "", EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction:
	default:
		return errors.New("environment must be development, staging, or production (omit when unknown)")
	}

	switch p.Auth {
	case "", "iam":
		if p.SecretID != "" {
			return errors.New("secret_id requires auth=secrets-manager; IAM is the default")
		}
	case AuthSecretsManager:
		if p.SecretID == "" {
			return errors.New("auth=secrets-manager requires secret_id (secret name or full ARN)")
		}
	default:
		return errors.New("auth must be iam or secrets-manager")
	}
	return nil
}
