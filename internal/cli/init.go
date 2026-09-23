package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/setup"
)

func initProfile(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(output)
	var p profile.Profile
	flags.StringVar(&p.RootCert, "sslrootcert", "", "optional custom CA PEM file (default: automatically managed AWS RDS bundle)")
	flags.StringVar(&p.Region, "region", "", "AWS region (defaults to AWS configuration)")
	flags.StringVar(&p.AWSProfile, "aws-profile", "", "AWS profile to use and save (defaults to current AWS credentials)")
	flags.StringVar(&p.AWSProfile, "profile", "", "alias for --aws-profile")
	path := flags.String("config", "", "destination JSON file (defaults to shared user configuration)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse setup options: %w", err)
	}
	if flags.NArg() != 0 {
		return ErrUsage
	}
	if *path == "" {
		var err error
		*path, err = profile.UserPath()
		if err != nil {
			return fmt.Errorf("select configuration destination: %w", err)
		}
	}
	// /dev/tty is not supported by Go's poller on every platform. Read it
	// nonblocking and wait with poll so cancellation never depends on Close.
	terminal, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("interactive setup needs a terminal: %w", err)
	}
	defer unix.Close(terminal) //nolint:errcheck // This input-only terminal has no buffered output.
	cfgCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg, err := awsConfig(cfgCtx, &p)
	if err != nil {
		return err
	}
	wizard := setup.Wizard{Input: terminalInput{fd: terminal, done: ctx.Done()}, Output: output, Config: cfg, AWSProfile: p.AWSProfile, RootCert: p.RootCert, Path: *path}
	wizard.Verify = func(verifyCtx context.Context, candidate *profile.Profile) error {
		return execute(verifyCtx, candidate, func(context.Context, []string) error { return nil }, log.New(output, "pg-tunnel: ", 0))
	}
	if err = wizard.Run(ctx); err != nil {
		return fmt.Errorf("initialize profile: %w", err)
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
	descriptors := []unix.PollFd{{Fd: int32(input.fd), Events: unix.POLLIN}} //nolint:gosec // Unix file descriptors are signed C ints.
	for {
		select {
		case <-input.done:
			return 0, context.Canceled
		default:
		}
		if _, err := unix.Poll(descriptors, 100); err != nil {
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
