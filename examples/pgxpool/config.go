// Package pgxexample demonstrates renewable pgx pools under pg-tunnel run.
// Copy this file into your application; it is an example, not a public SDK.
package pgxexample

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgpassfile"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config reads pg-tunnel's environment and configures routing and password renewal.
// Call it before setting any application-specific pool options or callbacks.
func Config() (*pgxpool.Config, error) {
	passfile := os.Getenv("PGPASSFILE")
	if os.Getenv("PGSERVICEFILE") == "" || passfile == "" {
		return nil, errors.New("launch the application with pg-tunnel run CONNECTION -- COMMAND")
	}
	config, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("read pg-tunnel service: %w", err)
	}
	conn := config.ConnConfig
	if conn.RuntimeParams["hostaddr"] != "127.0.0.1" || conn.TLSConfig == nil || conn.TLSConfig.InsecureSkipVerify || conn.TLSConfig.ServerName != conn.Host {
		return nil, errors.New("expected pg-tunnel loopback routing and verify-full TLS settings")
	}
	// pgx does not implement libpq's hostaddr. Route locally while retaining Host
	// for TLS verification and password lookup; never send hostaddr to PostgreSQL.
	delete(conn.RuntimeParams, "hostaddr")
	conn.Fallbacks = nil
	conn.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	conn.Password = ""
	// Capture the path once, not the password: each physical connection must read
	// the latest atomic replacement, even when the pool itself is long-lived.
	config.BeforeConnect = func(_ context.Context, conn *pgx.ConnConfig) error {
		passwords, readErr := pgpassfile.ReadPassfile(passfile)
		if readErr != nil {
			return fmt.Errorf("read pg-tunnel password file: %w", readErr)
		}
		conn.Password = passwords.FindPassword(conn.Host, strconv.Itoa(int(conn.Port)), conn.Database, conn.User)
		if conn.Password == "" {
			return errors.New("pg-tunnel password file has no matching credential")
		}
		return nil
	}
	return config, nil
}
