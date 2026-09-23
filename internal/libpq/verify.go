package libpq

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vertti/pg-tunnel/internal/session"
)

// Verify authenticates through the tunnel over verified TLS, without issuing SQL.
func Verify(ctx context.Context, target session.Target, port int, credential session.Credential) error {
	tlsConfig, err := TLSConfig(target)
	if err != nil {
		return err
	}
	config, err := verificationConfig(target, port, credential)
	if err != nil {
		return err
	}
	config.TLSConfig = tlsConfig
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgconn.ConnectConfig(verifyCtx, config)
	if err == nil {
		err = conn.Close(verifyCtx)
	}
	if err != nil {
		hint := "check CA, secret username/password, and database password authentication; RDS users with rds_iam must use IAM"
		if !credential.ExpiresAt.IsZero() {
			hint = "check CA, rds-db:connect, and database user rds_iam membership"
		}
		return fmt.Errorf("TLS/database connection failed (%s): %s", hint, strings.ReplaceAll(err.Error(), credential.Secret, "[redacted]"))
	}
	return nil
}

func verificationConfig(target session.Target, port int, credential session.Credential) (*pgconn.Config, error) {
	config, err := isolatedConfig()
	if err != nil {
		return nil, fmt.Errorf("prepare database verification: %w", err)
	}
	config.Host, config.Port = target.Host, uint16(port) //nolint:gosec // The port comes from a bound TCP listener.
	config.User, config.Database, config.Password = target.User, target.Database, credential.Secret
	config.RuntimeParams = map[string]string{"application_name": "pg-tunnel-verification"}
	config.Fallbacks = nil
	config.MaxProtocolMessageBodyLen = 1 << 20
	config.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	if !credential.ExpiresAt.IsZero() {
		config.RequireAuth = "password"
	}
	return config, nil
}

// TLSConfig loads the configured trust roots before any AWS session is opened.
func TLSConfig(target session.Target) (*tls.Config, error) {
	cert, err := os.ReadFile(target.RootCert)
	if err != nil {
		return nil, fmt.Errorf("read sslrootcert %s: %w", target.RootCert, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		return nil, errors.New("sslrootcert contains no usable PEM certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: target.Host}, nil
}

// pgconn always honors PGSERVICE, even when an empty service is passed. Supply our
// own empty, non-secret service file instead of mutating process environment.
func isolatedConfig() (config *pgconn.Config, result error) {
	dir, err := os.MkdirTemp("", "pg-tunnel-verification-")
	if err != nil {
		return nil, fmt.Errorf("create isolated verification settings: %w", err)
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	if writeErr := atomicWrite(dir, "service", "[pg-tunnel]\n"); writeErr != nil {
		return nil, writeErr
	}
	// A dummy password suppresses .pgpass reads. TLS is configured after parsing,
	// with certificate paths explicitly cleared to suppress ambient file reads.
	connection := "postgres://pg-tunnel:unused@127.0.0.1:5432/pg-tunnel?service=pg-tunnel&servicefile=" + url.QueryEscape(filepath.Join(dir, "service")) +
		"&sslmode=disable&sslrootcert=&sslcert=&sslkey=&sslpassword=&sslsni=1&sslnegotiation=postgres" +
		"&connect_timeout=10&target_session_attrs=any&min_protocol_version=3.0&max_protocol_version=3.0" +
		"&channel_binding=prefer&require_auth=password,md5,scram-sha-256"
	config, err = pgconn.ParseConfig(connection)
	if err != nil {
		return nil, fmt.Errorf("parse isolated verification settings: %w", err)
	}
	return config, nil
}
