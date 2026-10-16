// Package libpq publishes private, atomically refreshed PostgreSQL client files.
package libpq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/vertti/pg-tunnel/internal/atomicfile"
	"github.com/vertti/pg-tunnel/internal/session"
)

// Files creates isolated libpq settings in a directory owned by the current user.
// When a session gets its fixed LocalPort, it also keeps a block in the shared
// Pgpass file so apps started outside the session can connect to localhost.
type Files struct {
	Report    func(string)
	Root      string
	Pgpass    string
	LocalPort int
}

// client holds an advisory lock until credential cleanup finishes.
type client struct {
	err    error
	lock   *os.File
	dir    string
	pgpass string
	target session.Target
	port   int
	once   sync.Once
}

// Prepare creates a session and recovers abandoned sessions first.
func (f Files) Prepare(target session.Target, port int, credential session.Credential) (session.Client, error) {
	root, err := f.rootLock()
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // Closing this read-only directory only releases its advisory lock.
	if _, recoveryErr := f.recover(); recoveryErr != nil {
		return nil, recoveryErr
	}
	dir, err := os.MkdirTemp(f.Root, "session-")
	if err != nil {
		return nil, fmt.Errorf("create private session directory: %w", err)
	}
	lock, err := lockDirectory(dir)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	c := &client{target: target, port: port, dir: dir, lock: lock}
	if f.shares(port) {
		c.pgpass = f.Pgpass
	}
	if err = c.initialize(credential); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	if f.Report != nil {
		f.reportSharing(c)
	}
	return c, nil
}

func (f Files) reportSharing(c *client) {
	switch {
	case c.pgpass != "":
		f.Report(fmt.Sprintf("Postico, notebooks and scripts in any terminal can connect to host 127.0.0.1, port %d, database %q, user %q, SSL required, password left empty (%s holds the current one). Connection string:\n  %s", c.port, c.target.Database, c.target.User, displayPath(c.pgpass), c.connectionString()))
	case f.Pgpass != "" && f.LocalPort != 0:
		f.Report(fmt.Sprintf("%s is not updated because port %d is taken, so apps started elsewhere cannot connect with an empty password; stop whatever uses port %d and restart this session.", displayPath(f.Pgpass), f.LocalPort, f.LocalPort))
	}
}

func (f Files) shares(port int) bool {
	return f.Pgpass != "" && f.LocalPort != 0 && port == f.LocalPort
}

func (c *client) initialize(credential session.Credential) error {
	for _, value := range []string{c.target.Host, c.target.Database, c.target.User, c.target.RootCert} {
		if strings.ContainsAny(value, "\r\n\x00") || strings.TrimSpace(value) != value {
			return errors.New("connection settings cannot contain line breaks, NULs, or surrounding whitespace")
		}
	}
	service := fmt.Sprintf("[pg-tunnel]\nhost=%s\nhostaddr=127.0.0.1\nport=%d\ndbname=%s\nuser=%s\nsslmode=verify-full\nsslrootcert=%s\nconnect_timeout=10\n", c.target.Host, c.port, c.target.Database, c.target.User, c.target.RootCert)
	if err := atomicWrite(c.dir, "pg_service.conf", service); err != nil {
		return err
	}
	return c.Update(credential)
}

// Update replaces the password file without exposing an incomplete credential.
func (c *client) Update(credential session.Credential) error {
	if credential.Secret == "" || strings.ContainsAny(credential.Secret, "\r\n\x00") {
		return errors.New("credential is empty or contains unsupported control characters")
	}
	line := pgpassLine(c.target.Host, strconv.Itoa(c.port), c.target.Database, c.target.User, credential.Secret)
	if err := atomicWrite(c.dir, "pgpass", line+"\n"); err != nil {
		return err
	}
	if c.pgpass == "" {
		return nil
	}
	return editShared(c.pgpass, c.sharedLock(), filepath.Base(c.dir), c.sharedLines(credential.Secret))
}

func (c *client) sharedLock() string { return filepath.Join(filepath.Dir(c.dir), "pgpass.lock") }

// Env replaces inherited libpq settings while leaving unrelated variables alone.
func (c *client) Env(base []string) []string {
	env := make([]string, 0, len(base)+3)
	for _, entry := range base {
		if !strings.HasPrefix(entry, "PG") {
			env = append(env, entry)
		}
	}
	return append(env, "PGSERVICE=pg-tunnel", "PGSERVICEFILE="+filepath.Join(c.dir, "pg_service.conf"), "PGPASSFILE="+filepath.Join(c.dir, "pgpass"))
}

// Close removes credentials before releasing ownership and is safe to repeat.
func (c *client) Close() error {
	c.once.Do(func() {
		if c.pgpass != "" {
			c.err = editShared(c.pgpass, c.sharedLock(), filepath.Base(c.dir), nil)
		}
		c.err = errors.Join(c.err, os.RemoveAll(c.dir), c.lock.Close())
	})
	if c.err != nil {
		return fmt.Errorf("remove private client settings: %w", c.err)
	}
	return nil
}

// Recover removes abandoned session directories, skipping live locked sessions,
// and reports how many it removed.
func (f Files) Recover() (int, error) {
	root, err := f.rootLock()
	if err != nil {
		return 0, err
	}
	defer root.Close() //nolint:errcheck // Closing this read-only directory only releases its advisory lock.
	return f.recover()
}

func (f Files) recover() (int, error) {
	removed, err := recoverSessions(f.Root)
	if err != nil || f.Pgpass == "" {
		return removed, err
	}
	return removed, recoverShared(f.Pgpass, filepath.Join(f.Root, "pgpass.lock"), f.Root)
}

func (f Files) rootLock() (*os.File, error) {
	if err := os.MkdirAll(f.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create session storage: %w", err)
	}
	info, err := os.Lstat(f.Root)
	if err != nil {
		return nil, fmt.Errorf("inspect session storage: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("session storage must be a real directory with mode 0700")
	}
	root, err := os.Open(f.Root)
	if err != nil {
		return nil, fmt.Errorf("open session storage: %w", err)
	}
	if err = unix.Flock(int(root.Fd()), unix.LOCK_EX); err != nil {
		return nil, errors.Join(fmt.Errorf("lock session storage: %w", err), root.Close())
	}
	return root, nil
}

func lockDirectory(dir string) (*os.File, error) {
	file, err := os.Open(dir) //nolint:gosec // The directory is inside private, application-owned session storage.
	if err != nil {
		return nil, fmt.Errorf("open session directory: %w", err)
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("lock session directory: %w", err), file.Close())
	}
	return file, nil
}

func recoverSessions(root string) (removed int, _ error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("list abandoned sessions: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		lock, lockErr := lockDirectory(dir)
		if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, os.ErrNotExist) {
			continue
		}
		if lockErr != nil {
			return removed, lockErr
		}
		if err = errors.Join(os.RemoveAll(dir), lock.Close()); err != nil {
			return removed, fmt.Errorf("remove abandoned session: %w", err)
		}
		removed++
	}
	return removed, nil
}

func atomicWrite(dir, name, content string) error {
	if err := atomicfile.Write(filepath.Join(dir, name), []byte(content)); err != nil {
		return fmt.Errorf("publish private settings: %w", err)
	}
	return nil
}
