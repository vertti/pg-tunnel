package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
)

func runSession(ctx context.Context, mode string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(output)
	var path string
	flags.Func("config", "connection profiles (JSON); default: project pg-tunnel.json, then user configuration", func(value string) error {
		if value == "" {
			return errors.New("--config requires a non-empty path")
		}
		path = value
		return nil
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse session options: %w", err)
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return ErrUsage
	}
	command := remaining[1:]
	if mode == "run" {
		if len(command) < 2 || command[0] != "--" {
			return ErrUsage
		}
		command = command[1:]
	} else if len(command) > 0 {
		return ErrUsage
	}
	p, err := profile.Load(path, remaining[0])
	if err != nil {
		return fmt.Errorf("load connection profile: %w", err)
	}
	return execute(ctx, &p, command, output)
}

func execute(ctx context.Context, p *profile.Profile, command []string, output io.Writer) error {
	logger := log.New(output, "pg-tunnel: ", 0)
	report := func(message string) { logger.Print(message) }
	setupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg, err := awsConfig(setupCtx, p)
	if err != nil {
		return err
	}
	if p.RootCert == "" {
		p.RootCert, err = awsdb.RDSCA(setupCtx, cfg.Region, report)
		if err != nil {
			return fmt.Errorf("prepare RDS certificates: %w", err)
		}
	}
	if _, err = libpq.TLSConfig(session.Target{RootCert: p.RootCert}); err != nil {
		return fmt.Errorf("validate CA certificate: %w", err)
	}
	jump, err := awsdb.JumpHost(setupCtx, ec2.NewFromConfig(cfg), p)
	if err != nil {
		return fmt.Errorf("discover jump host: %w", err)
	}
	root, err := sessionRoot()
	if err != nil {
		return err
	}
	envExpiry, err := environmentExpiry()
	if err != nil {
		return err
	}
	runner := session.Runner{
		Resolver:  &awsdb.Resolver{API: rds.NewFromConfig(cfg), Profile: p},
		Transport: &awsdb.SSM{API: ssm.NewFromConfig(cfg), Region: cfg.Region, Profile: p.AWSProfile, Target: jump, LocalPort: p.LocalPort},
		Auth:      awsdb.IAM{Provider: cfg.Credentials, Region: cfg.Region, EnvironmentExpiry: envExpiry, Report: report},
		Clients:   libpq.Files{Root: root}, Verify: libpq.Verify, Env: os.Environ(), Report: report,
		Command: sessionCommand(command, report),
	}
	if err = runner.Run(ctx); err != nil {
		return fmt.Errorf("database session: %w", err)
	}
	return nil
}

func sessionCommand(command []string, report func(string)) session.Command {
	return func(ctx context.Context, env []string) error {
		if len(command) > 0 {
			return process.Run(ctx, command, env)
		}
		report("Client settings (keep this session running; Ctrl-C stops it):")
		for _, entry := range env {
			if len(entry) >= 2 && entry[:2] == "PG" {
				report(entry)
			}
		}
		<-ctx.Done()
		return fmt.Errorf("session stopped: %w", context.Cause(ctx))
	}
}

func awsConfig(ctx context.Context, p *profile.Profile) (aws.Config, error) {
	var options []func(*config.LoadOptions) error
	if p.Region != "" {
		options = append(options, config.WithRegion(p.Region))
	}
	if p.AWSProfile != "" {
		options = append(options, config.WithSharedConfigProfile(p.AWSProfile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return cfg, fmt.Errorf("load AWS configuration; check your profile or SSO login: %w", err)
	}
	if cfg.Region == "" {
		return cfg, errors.New("AWS region is missing; set region in the connection profile or AWS_REGION")
	}
	return cfg, nil
}

func environmentExpiry() (time.Time, error) {
	value := os.Getenv("AWS_CREDENTIAL_EXPIRATION")
	if value == "" || os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		return time.Time{}, nil
	}
	expiry, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse AWS_CREDENTIAL_EXPIRATION: %w", err)
	}
	return expiry, nil
}

func sessionRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	return filepath.Join(cache, "pg-tunnel", "sessions"), nil
}

func cleanup() error {
	root, err := sessionRoot()
	if err != nil {
		return err
	}
	if err = (libpq.Files{Root: root}).Recover(); err != nil {
		return fmt.Errorf("recover abandoned credentials: %w", err)
	}
	return nil
}
