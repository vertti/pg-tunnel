package libpq

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/atomicfile"
)

// localLines returns the password entry for clients that connect to 127.0.0.1.
// localhost is left out: it may resolve to ::1, where another local user can listen.
func (c *client) localLines(secret string) []string {
	return []string{pgpassLine("127.0.0.1", strconv.Itoa(c.port), c.target.Database, c.target.User, secret)}
}

func pgpassLine(fields ...string) string {
	escape := strings.NewReplacer("\\", "\\\\", ":", "\\:")
	for index := range fields {
		fields[index] = escape.Replace(fields[index])
	}
	return strings.Join(fields, ":")
}

func (c *client) connectionString() string {
	address := url.URL{Scheme: "postgresql", User: url.User(c.target.User), Host: "127.0.0.1:" + strconv.Itoa(c.port), Path: "/" + c.target.Database, RawQuery: "sslmode=require"}
	return address.String()
}

func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return path
}

func beginMarker(session string) string {
	return "# pg-tunnel " + session + " begin (managed; removed when the session ends)"
}

func endMarker(session string) string { return "# pg-tunnel " + session + " end" }

// editShared replaces the session's block at the top of the shared password file,
// or removes it when lines is nil, and leaves every other line unchanged.
func editShared(path, lockPath, session string, lines []string) error {
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDONLY, 0o600) //nolint:gosec // The lock lives in private session storage.
	if err != nil {
		return fmt.Errorf("open shared password file lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // Closing releases the advisory lock.
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock shared password file: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	data, err := os.ReadFile(path) //nolint:gosec // The path is the user's own password file.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	kept := withoutBlocks(string(data), func(name string) bool { return name == session })
	if lines == nil && kept == string(data) {
		return nil
	}
	var block string
	if lines != nil {
		block = beginMarker(session) + "\n" + strings.Join(lines, "\n") + "\n" + endMarker(session) + "\n"
	}
	if err = atomicfile.Write(path, []byte(block+kept)); err != nil {
		return fmt.Errorf("update %s: %w", path, err)
	}
	return nil
}

// withoutBlocks drops managed blocks whose session name matches remove.
func withoutBlocks(content string, remove func(string) bool) string {
	var kept strings.Builder
	skipping := ""
	for line := range strings.SplitAfterSeq(content, "\n") {
		if skipping != "" {
			if strings.TrimRight(line, "\n") == endMarker(skipping) {
				skipping = ""
			}
			continue
		}
		if name, ok := blockSession(line); ok && remove(name) {
			skipping = name
			continue
		}
		kept.WriteString(line)
	}
	if skipping != "" {
		return content
	}
	return kept.String()
}

func blockSession(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "# pg-tunnel ")
	if !ok {
		return "", false
	}
	name, rest, ok := strings.Cut(rest, " ")
	return name, ok && strings.HasPrefix(rest, "begin ")
}

// recoverShared removes blocks for sessions whose private directory is gone.
func recoverShared(path, lockPath, root string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // The path is the user's own password file.
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		name, ok := blockSession(line)
		if !ok || strings.ContainsAny(name, `/\`) {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, name)); errors.Is(statErr, os.ErrNotExist) {
			if editErr := editShared(path, lockPath, name, nil); editErr != nil {
				return editErr
			}
		}
	}
	return nil
}
