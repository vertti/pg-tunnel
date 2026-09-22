package session_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/session"
)

type resolverFunc func(context.Context) (session.Target, error)

func (f resolverFunc) Resolve(ctx context.Context) (session.Target, error) { return f(ctx) }

type authFunc func(context.Context, session.Target) (session.Credential, error)

func (f authFunc) Credential(ctx context.Context, target session.Target) (session.Credential, error) {
	return f(ctx, target)
}

type transportFunc func(context.Context, session.Target) (session.Tunnel, error)

func (f transportFunc) Open(ctx context.Context, target session.Target) (session.Tunnel, error) {
	return f(ctx, target)
}

type clientsFunc func(session.Target, int, session.Credential) (session.Client, error)

func (f clientsFunc) Prepare(target session.Target, port int, cred session.Credential) (session.Client, error) {
	return f(target, port, cred)
}

type fakeTunnel struct {
	done       chan struct{}
	closeError error
	onClose    func()
}

func (f *fakeTunnel) Port() int                   { return 15432 }
func (f *fakeTunnel) Done() <-chan struct{}       { return f.done }
func (*fakeTunnel) Err() error                    { return errors.New("lost SSM connection") }
func (f *fakeTunnel) Close(context.Context) error { f.onClose(); return f.closeError }

type fakeClient struct {
	onUpdate func(session.Credential) error
	onClose  func()
}

func (f *fakeClient) Update(c session.Credential) error { return f.onUpdate(c) }
func (*fakeClient) Env([]string) []string               { return []string{"PGSERVICE=pg-tunnel"} }
func (f *fakeClient) Close() error                      { f.onClose(); return nil }

func fixture() (session.Runner, *fakeTunnel, *fakeClient, *[]string) {
	var events []string
	tunnel := &fakeTunnel{done: make(chan struct{}), onClose: func() { events = append(events, "tunnel closed") }}
	client := &fakeClient{onClose: func() { events = append(events, "client closed") }, onUpdate: func(session.Credential) error { return nil }}
	runner := session.Runner{
		Resolver:  resolverFunc(func(context.Context) (session.Target, error) { return session.Target{Host: "db.example"}, nil }),
		Transport: transportFunc(func(context.Context, session.Target) (session.Tunnel, error) { return tunnel, nil }),
		Auth: authFunc(func(context.Context, session.Target) (session.Credential, error) {
			return session.Credential{Secret: "secret", ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
		}),
		Clients: clientsFunc(func(session.Target, int, session.Credential) (session.Client, error) { return client, nil }),
		Verify:  func(context.Context, session.Target, int, session.Credential) error { return nil },
		Command: func(context.Context, []string) error { return nil },
	}
	return runner, tunnel, client, &events
}

func TestStartupFailuresCleanOwnedResources(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"resolve", "transport", "auth", "verify", "client", "command"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			runner, _, _, events := fixture()
			failure := errors.New("injected failure")
			injectFailure(&runner, stage, failure)
			require.ErrorIs(t, runner.Run(t.Context()), failure)
			if stage == "resolve" || stage == "transport" {
				assert.Empty(t, *events)
				return
			}
			if stage == "command" {
				assert.Equal(t, []string{"client closed", "tunnel closed"}, *events)
				return
			}
			assert.Equal(t, []string{"tunnel closed"}, *events)
		})
	}
}

func injectFailure(r *session.Runner, stage string, failure error) {
	switch stage {
	case "resolve":
		r.Resolver = resolverFunc(func(context.Context) (session.Target, error) { return session.Target{}, failure })
	case "transport":
		r.Transport = transportFunc(func(context.Context, session.Target) (session.Tunnel, error) { return nil, failure })
	case "auth":
		r.Auth = authFunc(func(context.Context, session.Target) (session.Credential, error) {
			return session.Credential{}, failure
		})
	case "verify":
		r.Verify = func(context.Context, session.Target, int, session.Credential) error { return failure }
	case "client":
		r.Clients = clientsFunc(func(session.Target, int, session.Credential) (session.Client, error) { return nil, failure })
	case "command":
		r.Command = func(context.Context, []string) error { return failure }
	}
}

func TestCleanupDoesNotMaskCommandError(t *testing.T) {
	t.Parallel()
	runner, tunnel, _, events := fixture()
	commandErr, closeErr := errors.New("child failed"), errors.New("SSM termination denied")
	runner.Command = func(context.Context, []string) error { return commandErr }
	tunnel.closeError = closeErr
	err := runner.Run(t.Context())
	require.ErrorIs(t, err, commandErr)
	require.ErrorIs(t, err, closeErr)
	assert.Equal(t, []string{"client closed", "tunnel closed"}, *events)
}

func TestTunnelExitCancelsAndJoinsChild(t *testing.T) {
	t.Parallel()
	runner, tunnel, _, events := fixture()
	childStarted := make(chan struct{})
	runner.Command = func(ctx context.Context, _ []string) error { close(childStarted); <-ctx.Done(); return nil }
	result := make(chan error, 1)
	go func() { result <- runner.Run(t.Context()) }()
	<-childStarted
	close(tunnel.done)
	require.ErrorContains(t, <-result, "tunnel stopped")
	assert.Equal(t, []string{"client closed", "tunnel closed"}, *events)
}

func TestRefreshRetriesAndJoinsBeforeCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runner, _, client, events := fixture()
		var mu sync.Mutex
		calls := 0
		var published []string
		var messages []string
		runner.Report = func(message string) { mu.Lock(); defer mu.Unlock(); messages = append(messages, message) }
		runner.Auth = authFunc(func(context.Context, session.Target) (session.Credential, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 2 {
				return session.Credential{}, errors.New("credential source unavailable")
			}
			return session.Credential{Secret: "fresh", ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
		})
		client.onUpdate = func(c session.Credential) error {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, c.Secret)
			return nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runner.Command = func(ctx context.Context, _ []string) error { <-ctx.Done(); return nil }
		result := make(chan error, 1)
		go func() { result <- runner.Run(ctx) }()
		synctest.Wait()
		time.Sleep(12 * time.Minute)
		synctest.Wait()
		mu.Lock()
		assert.Equal(t, 2, calls)
		assert.Empty(t, published)
		assert.Contains(t, strings.Join(messages, "\n"), "retry in 5s")
		mu.Unlock()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		mu.Lock()
		assert.Equal(t, []string{"fresh"}, published)
		mu.Unlock()
		cancel()
		require.NoError(t, <-result)
		assert.Equal(t, []string{"client closed", "tunnel closed"}, *events)
	})
}

func TestShutdownCancelsBlockedRenewal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runner, _, _, events := fixture()
		calls := 0
		renewalStopped := false
		runner.Auth = authFunc(func(ctx context.Context, _ session.Target) (session.Credential, error) {
			calls++
			if calls == 1 {
				return session.Credential{ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
			}
			<-ctx.Done()
			renewalStopped = true
			return session.Credential{}, ctx.Err()
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runner.Command = func(ctx context.Context, _ []string) error { <-ctx.Done(); return nil }
		result := make(chan error, 1)
		go func() { result <- runner.Run(ctx) }()
		synctest.Wait()
		time.Sleep(12 * time.Minute)
		synctest.Wait()
		cancel()
		require.NoError(t, <-result)
		assert.True(t, renewalStopped)
		assert.Equal(t, []string{"client closed", "tunnel closed"}, *events)
	})
}

func TestCredentialPublicationFailureRetriesWithoutStoppingChild(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runner, _, client, _ := fixture()
		attempts := 0
		client.onUpdate = func(session.Credential) error {
			attempts++
			if attempts == 1 {
				return errors.New("disk full")
			}
			return nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		runner.Command = func(ctx context.Context, _ []string) error { <-ctx.Done(); return nil }
		result := make(chan error, 1)
		go func() { result <- runner.Run(ctx) }()
		synctest.Wait()
		time.Sleep(12*time.Minute + 5*time.Second)
		synctest.Wait()
		cancel()
		require.NoError(t, <-result)
		assert.Equal(t, 2, attempts)
	})
}

func TestInterruptedStartupClosesTunnel(t *testing.T) {
	t.Parallel()
	runner, _, _, events := fixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner.Verify = func(ctx context.Context, _ session.Target, _ int, _ session.Credential) error {
		cancel()
		return ctx.Err()
	}
	require.ErrorIs(t, runner.Run(ctx), context.Canceled)
	assert.Equal(t, []string{"tunnel closed"}, *events)
}
