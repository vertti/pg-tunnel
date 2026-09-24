package awsdb

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vertti/pg-tunnel/internal/atomicfile"
)

const maxBundleSize = 1 << 20

// RDSCA downloads and caches AWS's public RDS trust bundle. It checks for updates
// every 30 days; a failed refresh can use a still-valid cached bundle with a warning.
func RDSCA(ctx context.Context, region string, report func(string)) (string, error) {
	directory, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find RDS certificate cache: %w", err)
	}
	name, source := certificateSource(region)
	path := filepath.Join(directory, "pg-tunnel", "certificates", name)
	client := &http.Client{Timeout: 15 * time.Second}
	return cachedCA(ctx, client, path, source, report)
}

func certificateSource(region string) (name, source string) {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn.pem", "https://rds-truststore.s3.cn-north-1.amazonaws.com.cn/global/global-bundle.pem"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov.pem", "https://truststore.pki.us-gov-west-1.rds.amazonaws.com/global/global-bundle.pem"
	default:
		return "aws.pem", "https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem"
	}
}

func cachedCA(ctx context.Context, client *http.Client, path, source string, report func(string)) (string, error) {
	cached, readErr := os.ReadFile(path) //nolint:gosec // The path is inside the user's application cache.
	usable := readErr == nil && validateBundle(cached) == nil
	if usable && recentCA(path) {
		return path, nil
	}
	if report != nil {
		report("Downloading the RDS CA bundle from AWS (TLS verification remains enabled).")
	}
	data, err := downloadCA(ctx, client, source)
	if err != nil {
		if usable && ctx.Err() == nil {
			if report != nil {
				report(fmt.Sprintf("Warning: could not refresh the RDS CA bundle: %v; using the cached bundle. Server identity and certificate expiry are still verified.", err))
			}
			return path, nil
		}
		return "", fmt.Errorf("obtain RDS CA bundle; check HTTPS access to %s or configure sslrootcert with a trusted PEM file: %w", source, err)
	}
	if err := saveCA(path, data); err != nil {
		return "", err
	}
	return path, nil
}

func downloadCA(ctx context.Context, client *http.Client, source string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create certificate request: %w", err)
	}
	if request.URL.Scheme != "https" {
		return nil, errors.New("RDS CA downloads require HTTPS")
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := safeClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download certificate bundle: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // The response is read-only and fully validated before publication.
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("certificate download returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBundleSize+1))
	if err != nil {
		return nil, fmt.Errorf("read certificate download: %w", err)
	}
	if err := validateBundle(data); err != nil {
		return nil, err
	}
	return data, nil
}

func validateBundle(data []byte) error {
	if len(data) > maxBundleSize {
		return errors.New("RDS CA bundle exceeds 1 MiB")
	}
	currentCA := false
	for len(bytes.TrimSpace(data)) > 0 {
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return errors.New("RDS CA bundle contains invalid PEM data")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("parse RDS CA certificate: %w", err)
		}
		if !certificate.IsCA {
			return errors.New("RDS CA bundle contains a non-CA certificate")
		}
		now := time.Now()
		currentCA = currentCA || (!now.Before(certificate.NotBefore) && now.Before(certificate.NotAfter))
		data = rest
	}
	if !currentCA {
		return errors.New("RDS CA bundle contains no currently valid CA certificates")
	}
	return nil
}

func saveCA(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create certificate cache: %w", err)
	}
	if err := atomicfile.Write(path, data); err != nil {
		return fmt.Errorf("publish CA bundle: %w", err)
	}
	return nil
}

func recentCA(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime())
	return age >= 0 && age < 30*24*time.Hour
}
