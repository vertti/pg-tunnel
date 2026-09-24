package process

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// terminal lends the controlling terminal to a child process group and mirrors
// job control: when the child stops, the supervisor stops too so the shell regains
// the terminal, and resuming the supervisor resumes the child.
type terminal struct {
	mu         sync.Mutex
	fd         int
	supervisor int
}

// handover arranges for cmd to start in the foreground. It returns nil when stdin
// is not a terminal or the supervisor is a background job.
func handover(cmd *exec.Cmd, stdin io.Reader) *terminal {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	fd := int(file.Fd())
	foreground, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || foreground != syscall.Getpgrp() {
		return nil
	}
	cmd.SysProcAttr.Foreground, cmd.SysProcAttr.Ctty = true, fd
	return &terminal{fd: fd, supervisor: foreground}
}

func (t *terminal) foreground() int {
	pgrp, err := unix.IoctlGetInt(t.fd, unix.TIOCGPGRP)
	if err != nil {
		return -1
	}
	return pgrp
}

// give changes the foreground group; a background caller must not be stopped by SIGTTOU.
func (t *terminal) give(pgrp int) {
	signal.Ignore(syscall.SIGTTOU)
	_ = unix.IoctlSetPointerInt(t.fd, unix.TIOCSPGRP, pgrp) //nolint:errcheck // A vanished terminal cannot be repaired; process cleanup must still proceed.
	signal.Reset(syscall.SIGTTOU)
}

func (t *terminal) followStops(child int, sigchld <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-sigchld:
		}
		if stopped(child) {
			t.suspend(child)
		}
	}
}

func (t *terminal) suspend(child int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	resumed := make(chan os.Signal, 1)
	signal.Notify(resumed, syscall.SIGCONT)
	defer signal.Stop(resumed)
	t.give(t.supervisor)
	if err := syscall.Kill(0, syscall.SIGSTOP); err != nil {
		return
	}
	<-resumed
	if t.foreground() == t.supervisor {
		t.give(child)
	}
	_ = syscall.Kill(-child, syscall.SIGCONT) //nolint:errcheck // The child may already have exited.
}

// restore returns the terminal unless the shell has already taken it back.
func (t *terminal) restore(child int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.foreground() == child {
		t.give(t.supervisor)
	}
}
