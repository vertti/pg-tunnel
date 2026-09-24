package awsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"

	"github.com/vertti/pg-tunnel/internal/session"
)

// Secrets retrieves passwords from an explicitly selected Secrets Manager secret.
type Secrets struct {
	API *secretsmanager.Client
	ID  string
	// RequireHost rejects secrets that do not name the database they belong to.
	RequireHost bool
}

// Credential reads AWSCURRENT on every call, without persisting the secret JSON.
func (s Secrets) Credential(ctx context.Context, target session.Target) (session.Credential, error) {
	output, err := s.API.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(s.ID), VersionStage: aws.String("AWSCURRENT")})
	if err != nil {
		if ctx.Err() != nil {
			return session.Credential{}, fmt.Errorf("Secrets Manager request cancelled: %w", ctx.Err())
		}
		code := "request failed"
		var apiError smithy.APIError
		if errors.As(err, &apiError) {
			code = apiError.ErrorCode()
		}
		// Do not expose response bodies or decoder errors, which can contain secret data.
		return session.Credential{}, fmt.Errorf("Secrets Manager GetSecretValue for %q: %s; check AWS login, region, secretsmanager:GetSecretValue and (for a customer-managed key) kms:Decrypt", s.ID, code)
	}
	if output.SecretString == nil {
		return session.Credential{}, errors.New("secret must contain a JSON SecretString; binary secrets are not supported")
	}
	return passwordCredential(*output.SecretString, target, s.RequireHost)
}

type databaseSecret struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Host     string `json:"host"`
	Engine   string `json:"engine"`
	Port     int    `json:"port"`
}

func passwordCredential(value string, target session.Target, requireHost bool) (session.Credential, error) {
	var secret databaseSecret
	if err := json.Unmarshal([]byte(value), &secret); err != nil {
		return session.Credential{}, errors.New("secret must be a JSON object with string username/password fields and an optional numeric port")
	}
	if secret.Username != target.User || secret.Username == "" {
		return session.Credential{}, errors.New("secret username does not match the configured database user; choose the correct secret/user (alternating-user rotation is not supported)")
	}
	if secret.Password == "" || strings.ContainsAny(secret.Password, "\r\n\x00") {
		return session.Credential{}, errors.New("secret password is empty or contains characters unsupported by PostgreSQL password files")
	}
	if requireHost && secret.Host == "" {
		return session.Credential{}, errors.New("secret has no host field; a project pg-tunnel.json with an explicit host needs it to match, or select this file with --config to trust it")
	}
	if err := secret.validateEndpoint(target); err != nil {
		return session.Credential{}, err
	}
	return session.Credential{Secret: secret.Password}, nil
}

func (secret *databaseSecret) validateEndpoint(target session.Target) error {
	if secret.Engine != "" && secret.Engine != "postgres" {
		return errors.New("secret engine is not postgres")
	}
	if secret.Host != "" && !strings.EqualFold(strings.TrimSuffix(secret.Host, "."), strings.TrimSuffix(target.Host, ".")) {
		return errors.New("secret host does not match the configured database endpoint")
	}
	if secret.Port != 0 && secret.Port != target.Port {
		return errors.New("secret port does not match the configured database endpoint")
	}
	return nil
}
