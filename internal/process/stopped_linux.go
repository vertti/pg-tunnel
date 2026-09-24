package process

import (
	"bytes"
	"os"
	"strconv"
)

func stopped(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the parenthesized command name, which may itself contain ")".
	fields := bytes.Fields(stat[bytes.LastIndexByte(stat, ')')+1:])
	return len(fields) > 0 && (fields[0][0] == 'T' || fields[0][0] == 't')
}
