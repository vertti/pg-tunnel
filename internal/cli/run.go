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
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
)

func runSession(ctx context.Context, mode string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: pg-tunnel "+sessionSyntax[mode]) //nolint:errcheck // flag.Usage has no error return.
		flags.PrintDefaults()
	}
	var path string
	configFlag(flags, &path, "`PATH` of the JSON configuration; default: project pg-tunnel.json, then user configuration")
	if help, err := parseFlags(flags, args, stdout); help || err != nil {
		return err
	}
	name, command, err := sessionArguments(mode, path, flags.Args())
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
		return err //nolint:wrapcheck // Load errors already name the file and connection.
	}
	if mode == "check" {
		return checkConnection(ctx, &p, stderr)
	}
	report := reporter(stderr)
	return execute(ctx, &p, sessionCommand(command, report), report, mode == "connect")
}

var errNoAWSCredentials = errors.New("no AWS credentials found")

var verifyOnly session.Command = func(ctx context.Context, _ []string) error { return ctx.Err() }

func checkConnection(ctx context.Context, p *profile.Profile, output io.Writer) error {
	if err := execute(ctx, p, verifyOnly, reporter(output), false); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "Connection check passed; local tunnel and temporary credentials cleaned up."); err != nil {
		return fmt.Errorf("write check result: %w", err)
	}
	return nil
}

func reporter(output io.Writer) func(string) {
	logger := log.New(output, "pg-tunnel: ", 0)
	return func(message string) { logger.Print(message) }
}

var setupTimeout = 30 * time.Second

// configureRunner lets scenario tests replace the SSM transport and shorten polling.
var configureRunner = func(*session.Runner) {}

var sessionSyntax = map[string]string{
	"run":     "run [--config PATH] CONNECTION -- COMMAND [ARGS...]",
	"connect": "connect [--config PATH] CONNECTION",
	"check":   "check [--config PATH] CONNECTION",
}

func configFlag(flags *flag.FlagSet, path *string, usage string) {
	flags.Func("config", usage, func(value string) error {
		if value == "" {
			return errors.New("--config requires a non-empty path")
		}
		*path = value
		return nil
	})
}

func sessionArguments(mode, path string, args []string) (name string, command []string, err error) {
	if len(args) == 0 {
		return "", nil, missingConnection(mode, path)
	}
	name, rest := args[0], args[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "--" {
		option, _, _ := strings.Cut(rest[0], "=")
		return "", nil, usageError(mode, "put %s before the connection name", option)
	}
	if mode != "run" {
		if len(rest) > 0 {
			return "", nil, usageError(mode, "%s takes only a CONNECTION name", mode)
		}
		return name, nil, nil
	}
	switch {
	case len(rest) == 0:
		return "", nil, usageError(mode, "run needs a command: pg-tunnel run %s -- psql", name)
	case rest[0] != "--":
		return "", nil, usageError(mode, "put -- between the connection name and the command: pg-tunnel run %s -- %s", name, strings.Join(rest, " "))
	case len(rest) == 1:
		return "", nil, usageError(mode, "missing command after --")
	}
	return name, rest[1:], nil
}

func missingConnection(mode, path string) error {
	file, names, err := profile.Names(path)
	switch {
	case err != nil:
		return usageErr(fmt.Sprintf("%s needs a CONNECTION, but %v", mode, err))
	case len(names) == 0:
		return usageErr(fmt.Sprintf("%s needs a CONNECTION, but %s has none yet; create one with pg-tunnel init", mode, file))
	}
	return usageErr(fmt.Sprintf("%s needs a CONNECTION; available: %s", mode, strings.Join(names, ", ")))
}

func execute(ctx context.Context, p *profile.Profile, command session.Command, report func(string), shareAnyPort bool) error {
	if p.Environment == profile.EnvironmentProduction {
		report("WARNING: PRODUCTION connection. Database changes affect the production environment.")
	}
	setupCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	cfg, err := awsConfig(setupCtx, p)
	if errors.Is(err, errNoAWSCredentials) {
		return fmt.Errorf("%w; rerun with AWS_PROFILE=<profile>, or set aws_profile for this connection", err)
	}
	if err != nil {
		return err
	}
	err = prepareRootCert(setupCtx, p, cfg.Region, report)
	if err != nil {
		return err
	}
	jump, err := awsdb.JumpHost(setupCtx, ec2.NewFromConfig(cfg), p)
	if err != nil {
		return fmt.Errorf("discover jump host in %s: %w", cfg.Region, err)
	}
	files, err := sessionFiles()
	if err != nil {
		return err
	}
	files.LocalPort, files.AnyPort, files.Report = p.LocalPort, shareAnyPort, report
	authentication, err := databaseAuth(p, &cfg, report)
	if err != nil {
		return err
	}
	target, err := (&awsdb.Resolver{API: rds.NewFromConfig(cfg), Profile: p, Region: cfg.Region}).Resolve(setupCtx)
	if err != nil {
		return fmt.Errorf("discover database: %w", err)
	}
	runner := session.Runner{
		Target:    target,
		Transport: &awsdb.SSM{API: ssm.NewFromConfig(cfg), Region: cfg.Region, Profile: p.AWSProfile, Target: jump, LocalPort: p.LocalPort, Report: report},
		Auth:      authentication,
		Clients:   files, Verify: libpq.Verify, Env: os.Environ(), Report: report,
		Command: command,
	}
	configureRunner(&runner)
	if err = runner.Run(ctx); err != nil {
		return fmt.Errorf("database session: %w", err)
	}
	return nil
}

func prepareRootCert(ctx context.Context, p *profile.Profile, region string, report func(string)) error {
	if p.RootCert != "" {
		if _, err := libpq.TLSConfig(session.Target{RootCert: p.RootCert}); err != nil {
			return fmt.Errorf("validate CA certificate: %w", err)
		}
		return nil
	}
	var err error
	if p.RootCert, err = awsdb.RDSCA(ctx, region, report); err != nil {
		return fmt.Errorf("prepare RDS certificates: %w", err)
	}
	return nil
}

// sessionCommand runs command, or for connect waits; stopping connect with a
// signal is a normal exit.
func sessionCommand(command []string, report func(string)) session.Command {
	return func(ctx context.Context, env []string) error {
		if len(command) > 0 {
			return process.Run(ctx, command, env)
		}
		report("Keep this window open; Ctrl-C closes the tunnel.")
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
		err = credentialsError(&cfg, err)
		return cfg, err
	}
	return cfg, nil
}

func sharedConfig(cfg *aws.Config) config.SharedConfig {
	var shared config.SharedConfig
	for _, source := range cfg.ConfigSources {
		if value, ok := source.(config.SharedConfig); ok {
			shared = value
		}
	}
	return shared
}

func usesSSO(shared *config.SharedConfig) bool {
	return shared.SSOSessionName != "" || shared.SSOStartURL != ""
}

func credentialsError(cfg *aws.Config, err error) error {
	shared := sharedConfig(cfg)
	if usesSSO(&shared) {
		return fmt.Errorf("no usable AWS credentials; AWS profile %s uses IAM Identity Center; log in with aws sso login --profile %s: %w", shared.Profile, shared.Profile, err)
	}
	// The SDK falls back to the EC2 instance role only when nothing else configures credentials.
	if aws.IsCredentialsProvider(cfg.Credentials, &ec2rolecreds.Provider{}) {
		if shared.Profile != "" && shared.Profile != "default" {
			return fmt.Errorf("AWS profile %s has no credentials (aws configure list --profile %s)", shared.Profile, shared.Profile)
		}
		return errNoAWSCredentials
	}
	return fmt.Errorf("no usable AWS credentials; make AWS credentials available the way the AWS CLI finds them (environment, a named profile, or aws-vault): %w", err)
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

func sessionFiles() (libpq.Files, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return libpq.Files{}, fmt.Errorf("find user cache directory: %w", err)
	}
	files := libpq.Files{Root: filepath.Join(cache, "pg-tunnel", "sessions")}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		files.Pgpass = filepath.Join(home, ".pgpass")
	}
	return files, nil
}

func cleanupCommand(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: pg-tunnel cleanup\n\nRemove credential files left behind by crashed sessions.") //nolint:errcheck // flag.Usage has no error return.
	}
	if help, err := parseFlags(flags, args, stdout); help || err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("cleanup", "cleanup takes no arguments")
	}
	return cleanup(stderr)
}

func cleanup(output io.Writer) error {
	files, err := sessionFiles()
	if err != nil {
		return err
	}
	removed, err := files.Recover()
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
		return loginCheckedAuth{Auth: awsdb.Secrets{API: secretsmanager.NewFromConfig(*cfg), ID: p.SecretID, Region: cfg.Region}, cfg: cfg}, nil
	}
	expiry, err := environmentExpiry()
	if err != nil {
		return nil, err
	}
	return loginCheckedAuth{Auth: &awsdb.IAM{Provider: cfg.Credentials, Region: cfg.Region, EnvironmentExpiry: expiry, Report: report}, cfg: cfg}, nil
}

// loginCheckedAuth reports an expired AWS login as such, not as a failure of the AWS call it breaks.
type loginCheckedAuth struct {
	session.Auth
	cfg *aws.Config
}

// Credential checks the AWS login before asking the wrapped Auth for a credential.
func (a loginCheckedAuth) Credential(ctx context.Context, target session.Target) (session.Credential, error) {
	if _, err := a.cfg.Credentials.Retrieve(ctx); err != nil {
		shared := sharedConfig(a.cfg)
		var expired *ssocreds.InvalidTokenError
		switch {
		case usesSSO(&shared) && errors.As(err, &expired):
			return session.Credential{}, fmt.Errorf("AWS SSO login for profile %s has expired; run aws sso login --profile %s", shared.Profile, shared.Profile)
		case usesSSO(&shared):
			return session.Credential{}, fmt.Errorf("AWS SSO credentials for profile %s are unavailable; if aws sso login --profile %s does not help: %w", shared.Profile, shared.Profile, err)
		default:
			return session.Credential{}, fmt.Errorf("AWS credentials are unavailable: %w", err)
		}
	}
	return a.Auth.Credential(ctx, target) //nolint:wrapcheck // The wrapped Auth names its own AWS call.
}
