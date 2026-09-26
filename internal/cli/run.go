package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
	"github.com/vertti/pg-tunnel/internal/setup"
)

func runSession(ctx context.Context, mode string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: pg-tunnel "+sessionSyntax[mode]) //nolint:errcheck // flag.Usage has no error return.
		flags.PrintDefaults()
	}
	var path string
	flags.Func("config", "configuration file (JSON); default: project pg-tunnel.json, then user configuration", func(value string) error {
		if value == "" {
			return errors.New("--config requires a non-empty path")
		}
		path = value
		return nil
	})
	if help, err := parseFlags(flags, args, stdout, stderr); help || err != nil {
		return err
	}
	name, command, err := sessionArguments(mode, flags.Args())
	if err != nil {
		return err
	}
	if len(command) > 0 {
		if _, err = exec.LookPath(command[0]); err != nil {
			return fmt.Errorf("find command before connecting: %w", err)
		}
	}
	p, err := profile.Load(path, name)
	if err != nil {
		return fmt.Errorf("load connection: %w", err)
	}
	report := reporter(stderr)
	return execute(ctx, &p, sessionCommand(command, stdout, report), report)
}

func reporter(output io.Writer) func(string) {
	logger := log.New(output, "pg-tunnel: ", 0)
	return func(message string) { logger.Print(message) }
}

var sessionSyntax = map[string]string{
	"run":     "run [--config PATH] CONNECTION -- COMMAND [ARGS...]",
	"connect": "connect [--config PATH] CONNECTION",
}

func sessionArguments(mode string, args []string) (name string, command []string, err error) {
	if len(args) == 0 {
		return "", nil, usageError("%s needs a CONNECTION name: pg-tunnel %s", mode, sessionSyntax[mode])
	}
	name, rest := args[0], args[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "--config") {
		return "", nil, usageError("put --config before the connection name")
	}
	if mode == "connect" {
		if len(rest) > 0 {
			return "", nil, usageError("connect takes only a CONNECTION name")
		}
		return name, nil, nil
	}
	switch {
	case len(rest) == 0:
		return "", nil, usageError("run needs a command: pg-tunnel run %s -- psql", name)
	case rest[0] != "--":
		return "", nil, usageError("put -- between the connection name and the command: pg-tunnel run %s -- %s", name, strings.Join(rest, " "))
	case len(rest) == 1:
		return "", nil, usageError("missing command after --")
	}
	return name, rest[1:], nil
}

func execute(ctx context.Context, p *profile.Profile, command session.Command, report func(string)) error {
	if p.Environment == profile.EnvironmentProduction {
		report("WARNING: PRODUCTION connection. Database changes affect the production environment.")
	}
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
	authentication, err := databaseAuth(p, &cfg, report)
	if err != nil {
		return err
	}
	runner := session.Runner{
		Resolver:  &awsdb.Resolver{API: rds.NewFromConfig(cfg), Profile: p},
		Transport: &awsdb.SSM{API: ssm.NewFromConfig(cfg), Region: cfg.Region, Profile: p.AWSProfile, Target: jump, LocalPort: p.LocalPort, Report: report},
		Auth:      authentication,
		Clients:   libpq.Files{Root: root}, Verify: libpq.Verify, Env: os.Environ(), Report: report,
		Command: command,
	}
	if err = runner.Run(ctx); err != nil {
		return fmt.Errorf("database session: %w", err)
	}
	return nil
}

// sessionCommand runs command, or for connect prints shell exports of the client
// settings to stdout and waits; stopping connect with a signal is a normal exit.
func sessionCommand(command []string, stdout io.Writer, report func(string)) session.Command {
	return func(ctx context.Context, env []string) error {
		if len(command) > 0 {
			return process.Run(ctx, command, env)
		}
		report("Client settings for other terminals follow; keep this session running (Ctrl-C closes it).")
		for _, entry := range env {
			name, value, _ := strings.Cut(entry, "=")
			if !strings.HasPrefix(name, "PG") {
				continue
			}
			if _, err := fmt.Fprintf(stdout, "export %s=%s\n", name, setup.ShellQuote(value)); err != nil {
				return fmt.Errorf("write client settings: %w", err)
			}
		}
		<-ctx.Done()
		return nil
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
		if p.AWSProfile != "" {
			return cfg, fmt.Errorf("load AWS profile %q; check it exists (aws configure list-profiles) and its login: %w", p.AWSProfile, err)
		}
		return cfg, fmt.Errorf("load AWS configuration; check your AWS profile or SSO login: %w", err)
	}
	if cfg.Region == "" {
		return cfg, errors.New("AWS region is missing; set region in the AWS profile or connection, pass --region to init, or set AWS_REGION")
	}
	// Discovery errors would otherwise blame IAM permissions for a missing login.
	if _, err = cfg.Credentials.Retrieve(ctx); err != nil {
		loginErr := fmt.Errorf("no usable AWS credentials; %s: %w", loginHint(&cfg), err)
		return cfg, loginErr
	}
	return cfg, nil
}

func loginHint(cfg *aws.Config) string {
	for _, source := range cfg.ConfigSources {
		if shared, ok := source.(config.SharedConfig); ok && (shared.SSOSessionName != "" || shared.SSOStartURL != "") {
			return "AWS profile " + shared.Profile + " uses IAM Identity Center; log in with aws sso login --profile " + shared.Profile
		}
	}
	return "make AWS credentials available the way the AWS CLI finds them (environment, a named profile, or aws-vault) and select a profile with AWS_PROFILE or the connection's aws_profile if needed"
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

func cleanup(output io.Writer) error {
	root, err := sessionRoot()
	if err != nil {
		return err
	}
	removed, err := (libpq.Files{Root: root}).Recover()
	if err != nil {
		return fmt.Errorf("recover abandoned credentials: %w", err)
	}
	if _, err = fmt.Fprintf(output, "Removed %d abandoned session(s).\n", removed); err != nil {
		return fmt.Errorf("write cleanup summary: %w", err)
	}
	return nil
}

func databaseAuth(p *profile.Profile, cfg *aws.Config, report func(string)) (session.Auth, error) {
	if p.Auth == profile.AuthSecretsManager {
		return awsdb.Secrets{API: secretsmanager.NewFromConfig(*cfg), ID: p.SecretID}, nil
	}
	expiry, err := environmentExpiry()
	if err != nil {
		return nil, err
	}
	return awsdb.IAM{Provider: cfg.Credentials, Region: cfg.Region, EnvironmentExpiry: expiry, Report: report}, nil
}
