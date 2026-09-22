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

	"github.com/vertti/pg-tunnel/internal/session"
)

// Files creates isolated libpq settings in a directory owned by the current user.
type Files struct{ Root string }

// Client holds an advisory lock until credential cleanup finishes.
type Client struct {
	err    error
	lock   *os.File
	dir    string
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
	if recoveryErr := recoverSessions(f.Root); recoveryErr != nil {
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
	client := &Client{target: target, port: port, dir: dir, lock: lock}
	if err = client.initialize(credential); err != nil {
		return nil, errors.Join(err, client.Close())
	}
	return client, nil
}

func (c *Client) initialize(credential session.Credential) error {
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
func (c *Client) Update(credential session.Credential) error {
	if credential.Secret == "" || strings.ContainsAny(credential.Secret, "\r\n\x00") {
		return errors.New("credential is empty or contains unsupported control characters")
	}
	escape := strings.NewReplacer("\\", "\\\\", ":", "\\:")
	fields := []string{c.target.Host, strconv.Itoa(c.port), c.target.Database, c.target.User, credential.Secret}
	for index := range fields {
		fields[index] = escape.Replace(fields[index])
	}
	return atomicWrite(c.dir, "pgpass", strings.Join(fields, ":")+"\n")
}

// Env replaces inherited libpq settings while leaving unrelated variables alone.
func (c *Client) Env(base []string) []string {
	env := make([]string, 0, len(base)+3)
	for _, entry := range base {
		if !strings.HasPrefix(entry, "PG") {
			env = append(env, entry)
		}
	}
	return append(env, "PGSERVICE=pg-tunnel", "PGSERVICEFILE="+filepath.Join(c.dir, "pg_service.conf"), "PGPASSFILE="+filepath.Join(c.dir, "pgpass"))
}

// Close removes credentials before releasing ownership and is safe to repeat.
func (c *Client) Close() error {
	c.once.Do(func() { c.err = errors.Join(os.RemoveAll(c.dir), c.lock.Close()) })
	if c.err != nil {
		return fmt.Errorf("remove private client settings: %w", c.err)
	}
	return nil
}

// Recover removes abandoned session directories, skipping live locked sessions.
func (f Files) Recover() error {
	root, err := f.rootLock()
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // Closing this read-only directory only releases its advisory lock.
	return recoverSessions(f.Root)
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

func recoverSessions(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list abandoned sessions: %w", err)
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
			return lockErr
		}
		if err = errors.Join(os.RemoveAll(dir), lock.Close()); err != nil {
			return fmt.Errorf("remove abandoned session: %w", err)
		}
	}
	return nil
}

func atomicWrite(dir, name, content string) (result error) {
	file, err := os.CreateTemp(dir, ".credential-")
	if err != nil {
		return fmt.Errorf("create private replacement file: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove temporary credential file: %w", removeErr))
		}
	}()
	_, writeErr := file.WriteString(content)
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write private settings: %w", err)
	}
	if err = os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("publish private settings: %w", err)
	}
	return nil
}
