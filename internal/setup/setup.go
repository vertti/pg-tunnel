package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/libpq"
	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
)

// Wizard discovers metadata using the caller's AWS credentials. It never reads secret values.
type Wizard struct {
	Input      io.Reader
	Output     io.Writer
	Config     aws.Config
	AWSProfile string
	RootCert   string
	Path       string
}

type prompt struct {
	output io.Writer
	input  *bufio.Scanner
}

// Run previews a profile and saves it only after explicit confirmation.
// Input must honor cancellation; the CLI supplies a cancellable terminal reader.
func (w *Wizard) Run(ctx context.Context) error {
	ui := prompt{input: bufio.NewScanner(w.Input), output: w.Output}
	id, err := account(ctx, &w.Config)
	if err != nil {
		return err
	}
	if printErr := ui.print("AWS account %q, region %q. Discovering RDS PostgreSQL instances...\n", id, w.Config.Region); printErr != nil {
		return printErr
	}
	db, err := chooseDatabase(ctx, &ui, &w.Config)
	if err != nil {
		return err
	}
	target, err := chooseTarget(ctx, &ui, &w.Config, db)
	if err != nil {
		return err
	}
	p := profile.Profile{DBInstance: aws.ToString(db.DBInstanceIdentifier), Region: w.Config.Region, AWSProfile: w.AWSProfile, Target: target, RootCert: w.RootCert, Port: int(aws.ToInt32(db.Endpoint.Port))}
	if detailsErr := connectionDetails(&ui, &p, db); detailsErr != nil {
		return detailsErr
	}
	if p.RootCert == "" {
		if _, err = awsdb.RDSCA(ctx, w.Config.Region, func(message string) { log.New(w.Output, "", 0).Print(message) }); err != nil {
			return fmt.Errorf("prepare automatic RDS certificates: %w", err)
		}
	}
	return w.save(ctx, &ui, &p)
}

func chooseDatabase(ctx context.Context, ui *prompt, cfg *aws.Config) (*rdstypes.DBInstance, error) {
	dbs, err := databases(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if len(dbs) == 0 {
		return nil, fmt.Errorf("no RDS PostgreSQL instances found in %s; check the account and region", cfg.Region)
	}
	labels := make([]string, len(dbs))
	for i := range dbs {
		db := &dbs[i]
		labels[i] = fmt.Sprintf("%q (status %q, IAM enabled: %t)", aws.ToString(db.DBInstanceIdentifier), aws.ToString(db.DBInstanceStatus), aws.ToBool(db.IAMDatabaseAuthenticationEnabled))
	}
	index, err := ui.choose("Database", labels)
	if err != nil {
		return nil, err
	}
	db := &dbs[index]
	if err := describeDatabase(ui, db); err != nil {
		return nil, err
	}

	return db, nil
}

func describeDatabase(ui *prompt, db *rdstypes.DBInstance) error {
	if !aws.ToBool(db.IAMDatabaseAuthenticationEnabled) {
		return errors.New("selected database has IAM authentication disabled; pg-tunnel currently requires IAM database authentication")
	}
	if db.Endpoint == nil || aws.ToString(db.Endpoint.Address) == "" {
		return errors.New("selected database has no endpoint yet; wait until RDS has made it available")
	}
	if printErr := ui.print("Endpoint: %q\n", aws.ToString(db.Endpoint.Address)); printErr != nil {
		return printErr
	}
	if db.MasterUserSecret != nil {
		if printErr := ui.print("RDS-linked master-user secret: %q (metadata only; password authentication is not yet supported).\n", aws.ToString(db.MasterUserSecret.SecretArn)); printErr != nil {
			return printErr
		}
	}
	return nil
}

func chooseTarget(ctx context.Context, ui *prompt, cfg *aws.Config, db *rdstypes.DBInstance) (string, error) {
	var vpc string
	if db.DBSubnetGroup != nil {
		vpc = aws.ToString(db.DBSubnetGroup.VpcId)
	}
	hosts, err := jumpHosts(ctx, cfg, vpc)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("discovery cancelled: %w", ctx.Err())
		}
		if printErr := ui.print("Jump-host discovery unavailable: %v\nEnter an SSM target manually to continue.\n", err); printErr != nil {
			return "", printErr
		}
	}
	labels := make([]string, 0, len(hosts)+1)
	for i := range hosts {
		host := &hosts[i]
		labels = append(labels, fmt.Sprintf("%q %q (VPC %q, same VPC: %t)", aws.ToString(host.InstanceId), instanceName(host), aws.ToString(host.VpcId), vpc != "" && vpc == aws.ToString(host.VpcId)))
	}
	labels = append(labels, "Enter an SSM target manually")
	if printErr := ui.print("Online SSM hosts; matching VPC is a hint, not a connectivity check.\n"); printErr != nil {
		return "", printErr
	}
	index, err := ui.choose("Jump host", labels)
	if err != nil {
		return "", err
	}
	if index == len(hosts) {
		return ui.ask("SSM target ID", "")
	}
	return aws.ToString(hosts[index].InstanceId), nil
}

func connectionDetails(ui *prompt, p *profile.Profile, db *rdstypes.DBInstance) error {
	var err error
	if p.Database, err = ui.ask("Database name (AWS does not list databases inside the instance)", aws.ToString(db.DBName)); err != nil {
		return err
	}
	if p.User, err = ui.ask("IAM database user (must already exist with rds_iam membership)", ""); err != nil {
		return err
	}
	if p.RootCert == "" {
		if err = p.Validate(); err != nil {
			return fmt.Errorf("validate discovered profile: %w", err)
		}
		return nil
	}
	p.RootCert, err = filepath.Abs(p.RootCert)
	if err != nil {
		return fmt.Errorf("resolve CA path: %w", err)
	}
	if _, err = libpq.TLSConfig(session.Target{RootCert: p.RootCert}); err != nil {
		return fmt.Errorf("validate CA bundle: %w", err)
	}
	if err = p.Validate(); err != nil {
		return fmt.Errorf("validate discovered profile: %w", err)
	}
	return nil
}

func (w *Wizard) save(ctx context.Context, ui *prompt, p *profile.Profile) error {
	name, err := ui.ask("Profile name", p.DBInstance)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]any{"profiles": map[string]profile.Profile{name: *p}}, "", "  ")
	if err != nil {
		return fmt.Errorf("format profile preview: %w", err)
	}
	if printErr := ui.print("\n%s\n\nDestination: %q\nThis adds a profile; an existing name will not be replaced. Database login has not been tested.\n", data, w.Path); printErr != nil {
		return printErr
	}
	answer, err := ui.ask("Save this profile? (yes/no)", "no")
	if err != nil {
		return err
	}
	if !strings.EqualFold(answer, "yes") && !strings.EqualFold(answer, "y") {
		return ui.print("Cancelled; no configuration written.\n")
	}
	if err = ctx.Err(); err != nil {
		return fmt.Errorf("setup cancelled before saving: %w", err)
	}
	if err := profile.Save(w.Path, name, p); err != nil {
		return fmt.Errorf("save selected profile: %w", err)
	}
	return ui.print("Saved profile %q to %q. Verify access with:\n  pg-tunnel connect --config %s %s\n", name, w.Path, shellQuote(w.Path), shellQuote(name))
}

func (p *prompt) print(format string, args ...any) error {
	if _, err := fmt.Fprintf(p.output, format, args...); err != nil {
		return fmt.Errorf("write setup prompt: %w", err)
	}
	return nil
}

func (p *prompt) ask(label, fallback string) (string, error) {
	for {
		if err := p.print("%s [%s]: ", label, strconv.Quote(fallback)); err != nil {
			return "", err
		}
		if !p.input.Scan() {
			if err := p.input.Err(); err != nil {
				return "", fmt.Errorf("read setup answer: %w", err)
			}
			return "", errors.New("setup input ended; no configuration written")
		}
		answer := strings.TrimSpace(p.input.Text())
		if answer == "" {
			answer = fallback
		}
		if answer != "" {
			return answer, nil
		}
		if err := p.print("A value is required.\n"); err != nil {
			return "", err
		}
	}
}

func (p *prompt) choose(label string, choices []string) (int, error) {
	for i, choice := range choices {
		if err := p.print("  %d. %s\n", i+1, choice); err != nil {
			return 0, err
		}
	}
	for {
		answer, err := p.ask(label+" number", "")
		if err != nil {
			return 0, err
		}
		index, err := strconv.Atoi(answer)
		if err == nil && index >= 1 && index <= len(choices) {
			return index - 1, nil
		}
		if err := p.print("Choose a number from 1 to %d.\n", len(choices)); err != nil {
			return 0, err
		}
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
