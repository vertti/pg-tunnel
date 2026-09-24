package awsdb_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/pg-tunnel/internal/awsdb"
	"github.com/vertti/pg-tunnel/internal/session"
)

type secretHTTP func(*http.Request) (*http.Response, error)

func (f secretHTTP) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestSecretsValidateWithoutLeakingValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value, want string }{
		{"valid", `{"username":"reader","password":"test:password\\value","host":"DB.EXAMPLE.","port":5432,"engine":"postgres","dbname":"another"}`, ""},
		{"wrong user", `{"username":"other","password":"private-marker"}`, "username"},
		{"missing user", `{"password":"private-marker"}`, "username"},
		{"missing password", `{"username":"reader"}`, "password"},
		{"newline", `{"username":"reader","password":"private-marker\n"}`, "unsupported"},
		{"wrong host", `{"username":"reader","password":"private-marker","host":"other.example"}`, "host"},
		{"wrong port", `{"username":"reader","password":"private-marker","port":5433}`, "port"},
		{"wrong engine", `{"username":"reader","password":"private-marker","engine":"mysql"}`, "engine"},
		{"invalid JSON", `{"password":"private-marker"`, "JSON"},
		{"invalid type", `{"username":"reader","password":"private-marker","port":"private-marker"}`, "JSON"},
		{"null", `null`, "username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(map[string]string{"SecretString": tc.value})
			require.NoError(t, err)
			auth := secretAuth(t, string(payload), http.StatusOK)
			credential, err := auth.Credential(t.Context(), session.Target{Host: "db.example", Port: 5432, User: "reader"})
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				assert.NotContains(t, err.Error(), "private-marker")
				assert.Empty(t, credential.Secret)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, `test:password\value`, credential.Secret)
			assert.True(t, credential.ExpiresAt.IsZero())
		})
	}
}

func secretAuth(t *testing.T, payload string, status int) awsdb.Secrets {
	t.Helper()
	api := secretsmanager.NewFromConfig(aws.Config{Region: "eu-central-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1, HTTPClient: secretHTTP(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, request.Body.Close())
		assert.JSONEq(t, `{"SecretId":"chosen-secret","VersionStage":"AWSCURRENT"}`, string(body))
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
	})})
	return awsdb.Secrets{API: api, ID: "chosen-secret"}
}

func TestSecretsRequireHostRejectsSecretsWithoutHost(t *testing.T) {
	t.Parallel()
	target := session.Target{Host: "db.example", Port: 5432, User: "reader"}
	for _, tc := range []struct{ name, value, want string }{
		{"missing host", `{"username":"reader","password":"private-marker"}`, "no host field"},
		{"matching host", `{"username":"reader","password":"private-marker","host":"db.example"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(map[string]string{"SecretString": tc.value})
			require.NoError(t, err)
			auth := secretAuth(t, string(payload), http.StatusOK)
			auth.RequireHost = true
			credential, err := auth.Credential(t.Context(), target)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				assert.NotContains(t, err.Error(), "private-marker")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "private-marker", credential.Secret)
		})
	}
}

func TestSecretsErrorsDoNotExposeResponseBodies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, payload, want string
		status              int
	}{
		{"binary", `{"SecretBinary":"cHJpdmF0ZS1tYXJrZXI="}`, "binary secrets", http.StatusOK},
		{"denied", `{"__type":"AccessDeniedException","Message":"private-marker"}`, "AccessDeniedException", http.StatusBadRequest},
		{"malformed response", `{"SecretString":{"password":"private-marker"}}`, "request failed", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := secretAuth(t, tc.payload, tc.status).Credential(t.Context(), session.Target{User: "reader"})
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "private-marker")
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := secretAuth(t, `{}`, http.StatusOK).Credential(ctx, session.Target{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestSecretsReadsCurrentVersionOnEveryRefresh(t *testing.T) {
	t.Parallel()
	calls := 0
	api := secretsmanager.NewFromConfig(aws.Config{Region: "eu-central-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: secretHTTP(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, request.Body.Close())
		assert.JSONEq(t, `{"SecretId":"chosen-secret","VersionStage":"AWSCURRENT"}`, string(body))
		password := "first"
		if calls > 0 {
			password = "rotated"
		}
		calls++
		value, err := json.Marshal(map[string]string{"username": "reader", "password": password})
		require.NoError(t, err)
		payload, err := json.Marshal(map[string]string{"SecretString": string(value)})
		require.NoError(t, err)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(payload))), Request: request}, nil
	})})
	auth := awsdb.Secrets{API: api, ID: "chosen-secret"}
	first, err := auth.Credential(t.Context(), session.Target{User: "reader"})
	require.NoError(t, err)
	next, err := auth.Credential(t.Context(), session.Target{User: "reader"})
	require.NoError(t, err)
	assert.Equal(t, "first", first.Secret)
	assert.Equal(t, "rotated", next.Secret)
	assert.Equal(t, 2, calls)
}
