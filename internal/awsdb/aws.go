// Package awsdb implements RDS discovery, IAM authentication, and SSM transport.
package awsdb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/vertti/pg-tunnel/internal/profile"
	"github.com/vertti/pg-tunnel/internal/session"
)

// RDSAPI is the discovery operation used by Resolver.
type RDSAPI interface {
	DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
}

// EC2API resolves a jump-host tag without selecting an arbitrary match.
type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// Resolver resolves a configured RDS instance or an explicit endpoint.
type Resolver struct {
	API     RDSAPI
	Profile *profile.Profile
}

// Resolve returns the real endpoint used for IAM signing and TLS verification.
func (r *Resolver) Resolve(ctx context.Context) (session.Target, error) {
	target := session.Target{Host: r.Profile.Host, Port: r.Profile.Port, Database: r.Profile.Database, User: r.Profile.User, RootCert: r.Profile.RootCert}
	if target.Host != "" {
		return target, nil
	}
	output, err := r.API.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(r.Profile.DBInstance)})
	if err != nil {
		return target, fmt.Errorf("RDS DescribeDBInstances for %q (check region, profile, and rds:DescribeDBInstances permission): %w", r.Profile.DBInstance, err)
	}
	if len(output.DBInstances) != 1 {
		return target, fmt.Errorf("expected one RDS instance, found %d", len(output.DBInstances))
	}
	db := output.DBInstances[0]
	if aws.ToString(db.Engine) != "postgres" {
		return target, errors.New("the first AWS backend supports RDS PostgreSQL instances; use an explicit host for other PostgreSQL endpoints")
	}
	if !aws.ToBool(db.IAMDatabaseAuthenticationEnabled) {
		return target, errors.New("IAM database authentication is disabled on this RDS instance")
	}
	if db.Endpoint == nil || aws.ToString(db.Endpoint.Address) == "" || aws.ToInt32(db.Endpoint.Port) <= 0 {
		return target, errors.New("RDS did not return a usable database endpoint")
	}
	target.Host, target.Port = aws.ToString(db.Endpoint.Address), int(aws.ToInt32(db.Endpoint.Port))
	return target, nil
}

// JumpHost returns the configured instance or the unique running tag match.
func JumpHost(ctx context.Context, api EC2API, p *profile.Profile) (string, error) {
	if p.Target != "" {
		return p.Target, nil
	}
	input := &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:Name"), Values: []string{p.JumpTag}},
		{Name: aws.String("instance-state-name"), Values: []string{"running"}},
	}}
	var ids []string
	pages := ec2.NewDescribeInstancesPaginator(api, input)
	for pages.HasMorePages() {
		output, err := pages.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("EC2 DescribeInstances for Name=%q (check ec2:DescribeInstances permission): %w", p.JumpTag, err)
		}
		for _, reservation := range output.Reservations {
			for index := range reservation.Instances {
				ids = append(ids, aws.ToString(reservation.Instances[index].InstanceId))
			}
		}
	}
	if len(ids) != 1 || ids[0] == "" {
		return "", fmt.Errorf("expected one running jump host with Name=%q, found %d; configure an explicit target if ambiguous", p.JumpTag, len(ids))
	}
	return ids[0], nil
}

// IAM signs tokens using freshly retrieved AWS credentials on every renewal.
type IAM struct {
	EnvironmentExpiry time.Time
	Provider          aws.CredentialsProvider
	Report            func(string)
	Region            string
}

// Credential generates a token and conservatively bounds its usable lifetime.
func (a IAM) Credential(ctx context.Context, target session.Target) (session.Credential, error) {
	value, err := a.Provider.Retrieve(ctx)
	if err != nil {
		return session.Credential{}, fmt.Errorf("load AWS credentials; renew your AWS profile or SSO login: %w", err)
	}
	expiry := time.Now().Add(15 * time.Minute)
	var awsExpiry time.Time
	if value.Source == "EnvConfigCredentials" {
		awsExpiry = a.EnvironmentExpiry
	}
	if value.CanExpire {
		awsExpiry = value.Expires
	}
	if !awsExpiry.IsZero() && awsExpiry.Before(expiry) {
		expiry = awsExpiry
	}
	if time.Until(expiry) < time.Minute {
		return session.Credential{}, errors.New("AWS credentials expire in less than one minute; renew the AWS session (for aws-vault, use exec --server)")
	}
	if a.Report != nil {
		a.Report(credentialStatus(&value, awsExpiry))
	}
	provider := credentials.NewStaticCredentialsProvider(value.AccessKeyID, value.SecretAccessKey, value.SessionToken)
	secret, err := auth.BuildAuthToken(ctx, net.JoinHostPort(target.Host, strconv.Itoa(target.Port)), a.Region, target.User, provider)
	if err != nil {
		return session.Credential{}, fmt.Errorf("sign RDS IAM token: %w", err)
	}
	return session.Credential{Secret: secret, ExpiresAt: expiry}, nil
}

func credentialStatus(value *aws.Credentials, expiry time.Time) string {
	if !expiry.IsZero() {
		return "AWS credentials expire " + expiry.Format(time.RFC3339)
	}
	if value.SessionToken != "" {
		return "AWS temporary credential expiry is unknown; static environment credentials cannot be renewed. Use a refreshable profile or aws-vault exec --server."
	}
	return "AWS credential expiry is not reported by the provider."
}
