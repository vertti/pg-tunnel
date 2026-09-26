// Package session owns the lifecycle of a database connection environment.
package session

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Target separates the database identity from the local transport address.
type Target struct {
	Host     string
	Database string
	User     string
	RootCert string
	Port     int
}

// Credential is a renewable database authentication secret.
// Zero ExpiresAt denotes a password without a known expiry.
type Credential struct {
	ExpiresAt time.Time
	Secret    string
}

// String keeps credentials out of diagnostic output.
func (Credential) String() string { return "[redacted credential]" }

// GoString also redacts Go-syntax formatting.
func (c Credential) GoString() string { return c.String() }

// Resolver discovers the database endpoint.
type Resolver interface {
	Resolve(context.Context) (Target, error)
}

// Auth obtains a fresh credential using the current identity.
type Auth interface {
	Credential(context.Context, Target) (Credential, error)
}

// Tunnel owns a transport and its eventual exit status; Err is non-nil once Done closes.
type Tunnel interface {
	Port() int
	Done() <-chan struct{}
	Err() error
	Close(context.Context) error
}

// Transport opens a supervised local connection to a database.
type Transport interface {
	Open(context.Context, Target) (Tunnel, error)
}

// Client owns per-session client configuration.
type Client interface {
	Update(Credential) error
	Env([]string) []string
	Close() error
}

// Clients creates client-specific connection settings.
type Clients interface {
	Prepare(Target, int, Credential) (Client, error)
}

// Verify checks authentication and optionally reports access warnings.
// A nil reporter skips inspection during credential refresh.
type Verify func(context.Context, Target, int, Credential, func(string)) error

// Command runs the user's process and waits for its exit.
type Command func(context.Context, []string) error

// Runner composes providers while retaining sole ownership of cleanup.
type Runner struct {
	Resolver  Resolver
	Transport Transport
	Auth      Auth
	Clients   Clients
	Verify    Verify
	Command   Command
	Report    func(string)
	Env       []string
}

// Run starts a database session, runs its command, and closes every resource.
func (r *Runner) Run(ctx context.Context) (result error) {
	startupCtx, startupCancel := context.WithTimeout(ctx, time.Minute)
	defer startupCancel()
	target, err := r.Resolver.Resolve(startupCtx)
	if err != nil {
		return fmt.Errorf("discover database: %w", err)
	}
	r.report(fmt.Sprintf("Database: %s:%d (%s, user %s)", target.Host, target.Port, target.Database, target.User))

	tunnel, err := r.Transport.Open(startupCtx, target)
	if err != nil {
		return fmt.Errorf("open tunnel: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		result = errors.Join(result, tunnel.Close(cleanupCtx))
	}()

	credential, err := r.Auth.Credential(startupCtx, target)
	if err != nil {
		return fmt.Errorf("obtain database credential: %w", err)
	}
	if err = r.Verify(startupCtx, target, tunnel.Port(), credential, r.report); err != nil {
		return fmt.Errorf("verify database authentication: %w", err)
	}

	client, err := r.Clients.Prepare(target, tunnel.Port(), credential)
	if err != nil {
		return fmt.Errorf("prepare client settings: %w", err)
	}
	defer func() { result = errors.Join(result, client.Close()) }()
	r.report(fmt.Sprintf("Database authentication verified; tunnel: 127.0.0.1:%d", tunnel.Port()))
	r.reportExpiry(credential)
	startupCancel()
	return r.runCommand(ctx, target, tunnel, client, credential)
}

func (r *Runner) runCommand(ctx context.Context, target Target, tunnel Tunnel, client Client, credential Credential) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		r.refresh(runCtx, target, tunnel.Port(), client, credential)
	}()
	defer func() { cancel(nil); <-refreshDone }()

	commandDone := make(chan error, 1)
	go func() { commandDone <- r.Command(runCtx, client.Env(r.Env)) }()
	select {
	case err := <-commandDone:
		return err
	case <-tunnel.Done():
		err := fmt.Errorf("tunnel stopped; child command is being stopped: %w", tunnel.Err())
		cancel(err)
		return errors.Join(err, <-commandDone)
	}
}

const (
	passwordPoll   = 5 * time.Minute
	renewalMargin  = 3 * time.Minute
	minimumRenewal = 30 * time.Second
	// Monotonic timers pause while macOS sleeps, so waits are re-checked against the wall clock.
	wallClockCheck = time.Minute
)

func (r *Runner) refresh(ctx context.Context, target Target, port int, client Client, current Credential) {
	due := renewalTime(current)
	backoff := 5 * time.Second
	for {
		if !sleepUntil(ctx, due) {
			return
		}
		next, err := r.renew(ctx, target, port, client, current)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			due = time.Now().Round(0).Add(backoff)
			r.report(fmt.Sprintf("Credential refresh failed: %v. %s Retry in %s.", err, credentialValidity(current), backoff))
			backoff = min(backoff*2, time.Minute)
			continue
		}
		current = next
		backoff = 5 * time.Second
		due = renewalTime(current)
		r.reportExpiry(current)
	}
}

// sleepUntil waits for a wall-clock deadline and reports whether it was reached.
func sleepUntil(ctx context.Context, due time.Time) bool {
	for {
		wait := time.Until(due)
		if wait <= 0 {
			return true
		}
		timer := time.NewTimer(min(wait, wallClockCheck))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (r *Runner) renew(ctx context.Context, target Target, port int, client Client, current Credential) (Credential, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	credential, err := r.Auth.Credential(refreshCtx, target)
	if err != nil {
		return Credential{}, fmt.Errorf("obtain replacement credential: %w", err)
	}
	if credential.ExpiresAt.IsZero() && current.ExpiresAt.IsZero() && credential.Secret == current.Secret {
		return credential, nil
	}
	if err = r.Verify(refreshCtx, target, port, credential, nil); err != nil {
		return Credential{}, fmt.Errorf("verify replacement credential; previous credential retained: %w", err)
	}
	if err = client.Update(credential); err != nil {
		return Credential{}, fmt.Errorf("publish replacement credential: %w", err)
	}
	return credential, nil
}

// renewalTime has no monotonic reading, so comparisons use the wall clock.
func renewalTime(credential Credential) time.Time {
	now := time.Now().Round(0)
	if credential.ExpiresAt.IsZero() {
		return now.Add(passwordPoll)
	}
	earliest := now.Add(minimumRenewal)
	if renewal := credential.ExpiresAt.Round(0).Add(-renewalMargin); renewal.After(earliest) {
		return renewal
	}
	return earliest
}

func (r *Runner) report(message string) {
	if r.Report != nil {
		r.Report(message)
	}
}

func (r *Runner) reportExpiry(credential Credential) {
	if credential.ExpiresAt.IsZero() {
		r.report(fmt.Sprintf("Secrets Manager password loaded; expiry is not reported. Checking for rotation every %s. New connections may fail between rotation and refresh.", passwordPoll))
		return
	}
	r.report(fmt.Sprintf("IAM token expires %s; refresh in %s. Token expiry does not close established connections.", credential.ExpiresAt.Format(time.RFC3339), time.Until(renewalTime(credential)).Round(time.Second)))
}

func credentialValidity(credential Credential) string {
	if credential.ExpiresAt.IsZero() {
		return "The previous password is still published; it may no longer be valid after rotation."
	}
	return "Last published token expires " + credential.ExpiresAt.Format(time.RFC3339) + "; expiry does not close established connections."
}
