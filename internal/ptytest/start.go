package ptytest

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Start runs command in a new session whose controlling terminal is a new
// pseudo-terminal, and returns that terminal's primary.
func Start(command *exec.Cmd) (*os.File, error) {
	primary, replicaName, err := Open()
	if err != nil {
		return nil, err
	}
	replica, err := os.OpenFile(replicaName, os.O_RDWR|syscall.O_NOCTTY, 0) //nolint:gosec // The replica of the terminal opened above.
	if err != nil {
		return nil, closeOnError(fmt.Errorf("open pseudo-terminal replica: %w", err), primary)
	}
	defer replica.Close() //nolint:errcheck // The child holds its own descriptors.
	command.Stdin, command.Stdout, command.Stderr = replica, replica, replica
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err = command.Start(); err != nil {
		return nil, closeOnError(fmt.Errorf("start %s: %w", command.Path, err), primary)
	}
	return primary, nil
}
