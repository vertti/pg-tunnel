package process_test

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (primary *os.File, replicaName string, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open pseudo-terminal: %w", err)
	}
	primary = os.NewFile(uintptr(fd), "/dev/ptmx")
	if err = unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return nil, "", errorsJoinClose(fmt.Errorf("configure pseudo-terminal: %w", err), primary)
	}
	if err = unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return nil, "", errorsJoinClose(fmt.Errorf("configure pseudo-terminal: %w", err), primary)
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 { //nolint:gosec // TIOCPTYGNAME writes the replica path into this fixed buffer.
		return nil, "", errorsJoinClose(fmt.Errorf("name pseudo-terminal: %w", errno), primary)
	}
	return primary, string(name[:bytes.IndexByte(name[:], 0)]), nil
}
