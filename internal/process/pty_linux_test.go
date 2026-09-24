package process_test

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func openPTY() (primary *os.File, replicaName string, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open pseudo-terminal: %w", err)
	}
	primary = os.NewFile(uintptr(fd), "/dev/ptmx")
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return nil, "", errorsJoinClose(fmt.Errorf("configure pseudo-terminal: %w", err), primary)
	}
	number, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		return nil, "", errorsJoinClose(fmt.Errorf("configure pseudo-terminal: %w", err), primary)
	}
	return primary, "/dev/pts/" + strconv.FormatUint(uint64(number), 10), nil
}
