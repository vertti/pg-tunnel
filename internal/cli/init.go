package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/setup"
)

func initProfile(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: pg-tunnel init [--aws-profile PROFILE] [--region REGION] [--config PATH] [--sslrootcert PEM]") //nolint:errcheck // flag.Usage has no error return.
		flags.PrintDefaults()
	}
	var p profile.Profile
	flags.StringVar(&p.RootCert, "sslrootcert", "", "optional custom CA PEM file (default: automatically managed AWS RDS bundle)")
	flags.StringVar(&p.Region, "region", "", "AWS region (defaults to AWS configuration)")
	flags.StringVar(&p.AWSProfile, "aws-profile", "", "AWS profile to use and save (defaults to current AWS credentials)")
	flags.StringVar(&p.AWSProfile, "profile", "", "alias for --aws-profile")
	path := flags.String("config", "", "destination JSON file (defaults to shared user configuration)")
	if help, err := parseFlags(flags, args, stdout); help || err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return usageError("init takes no arguments; pass the AWS profile with --aws-profile")
	}
	if *path == "" {
		var err error
		*path, err = profile.UserPath()
		if err != nil {
			return fmt.Errorf("select configuration destination: %w", err)
		}
	}
	// /dev/tty is not supported by Go's poller on every platform. Read it
	// nonblocking and wait with select so cancellation never depends on Close.
	terminal, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("init is interactive and needs a terminal; without one, write pg-tunnel.json by hand: %w", err)
	}
	defer unix.Close(terminal) //nolint:errcheck // This input-only terminal has no buffered output.
	cfgCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg, err := awsConfig(cfgCtx, &p)
	if err != nil {
		return err
	}
	wizard := setup.Wizard{Input: terminalInput{fd: terminal, done: ctx.Done()}, Output: stderr, Config: cfg, AWSProfile: p.AWSProfile, RootCert: p.RootCert, Path: *path}
	wizard.Verify = func(verifyCtx context.Context, candidate *profile.Profile) error {
		return execute(verifyCtx, candidate, func(context.Context, []string) error { return nil }, reporter(stderr))
	}
	if err = wizard.Run(ctx); err != nil {
		return fmt.Errorf("initialize connection: %w", err)
	}
	return nil
}

// terminalInput waits at most 100 ms before observing cancellation, including on
// macOS where /dev/tty cannot be registered with Go's runtime poller.
type terminalInput struct {
	done <-chan struct{}
	fd   int
}

func (input terminalInput) Read(buffer []byte) (int, error) {
	for {
		select {
		case <-input.done:
			return 0, context.Canceled
		default:
		}
		// macOS poll reports /dev/tty as always ready, which would spin; select does not.
		var readable unix.FdSet
		readable.Set(input.fd)
		if _, err := unix.Select(input.fd+1, &readable, nil, nil, &unix.Timeval{Usec: 100_000}); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, fmt.Errorf("wait for terminal input: %w", err)
		}
		count, err := unix.Read(input.fd, buffer)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return count, fmt.Errorf("read terminal: %w", err)
		}
		return count, nil
	}
}
