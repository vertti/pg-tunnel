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

// SecretsAPI reads the selected secret version.
type SecretsAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// Secrets retrieves passwords from an explicitly selected Secrets Manager secret.
type Secrets struct {
	API    SecretsAPI
	ID     string
	Region string
}

// Credential reads AWSCURRENT on every call, without persisting the secret JSON.
func (s Secrets) Credential(ctx context.Context, target session.Target) (session.Credential, error) {
	output, err := s.API.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(s.ID), VersionStage: aws.String("AWSCURRENT")})
	if err != nil {
		if problem, ok := transportProblem(err); ok {
			return session.Credential{}, &callError{text: fmt.Sprintf("Secrets Manager GetSecretValue for %q %s", s.ID, problem), err: err}
		}
		// Do not expose response bodies or decoder errors, which can contain secret data.
		var apiError smithy.APIError
		if !errors.As(err, &apiError) {
			return session.Credential{}, fmt.Errorf("Secrets Manager GetSecretValue for %q: request failed; check AWS login, region, secretsmanager:GetSecretValue and (for a customer-managed key) kms:Decrypt", s.ID)
		}
		switch code := apiError.ErrorCode(); code {
		case "AccessDeniedException":
			return session.Credential{}, fmt.Errorf("Secrets Manager GetSecretValue for %q was denied (AccessDeniedException); the AWS role needs secretsmanager:GetSecretValue on the secret, and kms:Decrypt if it uses a customer-managed key", s.ID)
		case "ResourceNotFoundException":
			return session.Credential{}, fmt.Errorf("Secrets Manager secret %q not found in %s; check secret_id, region and AWS profile", s.ID, s.Region)
		default:
			return session.Credential{}, fmt.Errorf("Secrets Manager GetSecretValue for %q failed: %s", s.ID, code)
		}
	}
	if output.SecretString == nil {
		return session.Credential{}, errors.New("secret must contain a JSON SecretString; binary secrets are not supported")
	}
	credential, err := passwordCredential(*output.SecretString, target)
	if err != nil {
		return session.Credential{}, err
	}
	credential.CreatedAt = aws.ToTime(output.CreatedDate)
	return credential, nil
}

type databaseSecret struct {
	Username string      `json:"username"`
	Password string      `json:"password"`
	Host     string      `json:"host"`
	Engine   string      `json:"engine"`
	Port     json.Number `json:"port"`
}

func passwordCredential(value string, target session.Target) (session.Credential, error) {
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
	if err := secret.validateEndpoint(target); err != nil {
		return session.Credential{}, err
	}
	return session.Credential{Secret: secret.Password}, nil
}

func (secret *databaseSecret) validateEndpoint(target session.Target) error {
	if secret.Engine != "" && !PostgreSQLEngine(secret.Engine) {
		return errors.New("secret engine is not postgres")
	}
	if secret.Host != "" && !strings.EqualFold(strings.TrimSuffix(secret.Host, "."), strings.TrimSuffix(target.Host, ".")) {
		return errors.New("secret host does not match the configured database endpoint")
	}
	if port, err := secret.Port.Int64(); secret.Port != "" && (err != nil || port != int64(target.Port)) {
		return errors.New("secret port does not match the configured database endpoint")
	}
	return nil
}
