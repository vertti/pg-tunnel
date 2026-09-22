// Package profile reads non-secret, project-local connection profiles.
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

// Profile configures an AWS IAM database session without storing credentials.
type Profile struct {
	DBInstance string `json:"db_instance"`
	Host       string `json:"host"`
	Database   string `json:"database"`
	User       string `json:"user"`
	Target     string `json:"target"`
	JumpTag    string `json:"jump_tag"`
	Region     string `json:"region"`
	AWSProfile string `json:"aws_profile"`
	RootCert   string `json:"sslrootcert"`
	Port       int    `json:"port"`
	LocalPort  int    `json:"local_port"`
}

// Load reads one named profile; certificate paths are relative to its file.
func Load(path, name string) (Profile, error) {
	file, err := os.Open(path) //nolint:gosec // The path is the configuration file explicitly selected by the user.
	if err != nil {
		return Profile{}, fmt.Errorf("open profiles %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // This file is read-only.
	var config struct {
		Profiles map[string]Profile `json:"profiles"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return Profile{}, fmt.Errorf("decode profiles: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Profile{}, errors.New("profiles must contain exactly one JSON object")
	}
	value, ok := config.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("profile %q does not exist in %s", name, path)
	}
	if value.Port == 0 {
		value.Port = 5432
	}
	if err = value.Validate(); err != nil {
		return Profile{}, fmt.Errorf("profile %q: %w", name, err)
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
	if p.Database == "" || p.User == "" || p.RootCert == "" {
		return errors.New("database, user, and sslrootcert are required")
	}
	if p.LocalPort < 0 || p.LocalPort > 65535 || p.Port < 1 || p.Port > 65535 {
		return errors.New("port must be 1–65535; local_port must be 0–65535 (0 selects an available port)")
	}
	return p.validateText()
}

func (p *Profile) validateText() error {
	for _, value := range []string{p.DBInstance, p.Host, p.Database, p.User, p.Target, p.JumpTag, p.Region, p.AWSProfile, p.RootCert} {
		if strings.ContainsAny(value, "\r\n\x00") || value != strings.TrimSpace(value) {
			return errors.New("profile values cannot contain line breaks, NULs, or surrounding whitespace")
		}
	}
	if strings.ContainsAny(p.Host, "/:, \\*") || strings.ContainsAny(p.Database, "*") || strings.ContainsAny(p.User, "*") {
		return errors.New("host must be a single DNS name; database and user cannot contain password-file wildcards")
	}
	return nil
}
