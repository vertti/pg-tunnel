// Package setup helps choose AWS resources and save a connection profile.
package setup

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/vertti/pg-tunnel/internal/awsdb"
)

func account(ctx context.Context, cfg *aws.Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := sts.NewFromConfig(*cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("identify AWS account; check your AWS profile or renew your login: %w", err)
	}
	return aws.ToString(result.Account), nil
}

func databases(ctx context.Context, cfg *aws.Config) ([]rdstypes.DBInstance, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pages := rds.NewDescribeDBInstancesPaginator(rds.NewFromConfig(*cfg), &rds.DescribeDBInstancesInput{})
	var result []rdstypes.DBInstance
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list RDS databases; check rds:DescribeDBInstances permission in %s: %w", cfg.Region, err)
		}
		for i := range page.DBInstances {
			db := &page.DBInstances[i]
			if awsdb.PostgreSQLEngine(aws.ToString(db.Engine)) {
				result = append(result, *db)
			}
		}
	}
	slices.SortFunc(result, func(a, b rdstypes.DBInstance) int {
		return strings.Compare(aws.ToString(a.DBInstanceIdentifier), aws.ToString(b.DBInstanceIdentifier))
	})
	return result, nil
}

func onlineNodes(ctx context.Context, cfg *aws.Config) (map[string]bool, error) {
	pages := ssm.NewDescribeInstanceInformationPaginator(ssm.NewFromConfig(*cfg), &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("PingStatus"), Values: []string{"Online"}}},
	})
	result := make(map[string]bool)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list online SSM nodes; check ssm:DescribeInstanceInformation permission: %w", err)
		}
		for i := range page.InstanceInformationList {
			node := &page.InstanceInformationList[i]
			result[aws.ToString(node.InstanceId)] = node.PingStatus == ssmtypes.PingStatusOnline
		}
	}
	return result, nil
}

func jumpHosts(ctx context.Context, cfg *aws.Config, vpc string) ([]ec2types.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	online, err := onlineNodes(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pages := ec2.NewDescribeInstancesPaginator(ec2.NewFromConfig(*cfg), &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{Name: aws.String("instance-state-name"), Values: []string{"running"}}},
	})
	var result []ec2types.Instance
	for pages.HasMorePages() {
		page, pageErr := pages.NextPage(ctx)
		if pageErr != nil {
			return nil, fmt.Errorf("list jump hosts; check ec2:DescribeInstances permission: %w", pageErr)
		}
		for _, reservation := range page.Reservations {
			for i := range reservation.Instances {
				instance := &reservation.Instances[i]
				if online[aws.ToString(instance.InstanceId)] {
					result = append(result, *instance)
				}
			}
		}
	}
	sortHosts(result, vpc)
	return result, nil
}

func sortHosts(result []ec2types.Instance, vpc string) {
	slices.SortFunc(result, func(a, b ec2types.Instance) int {
		aMatch, bMatch := aws.ToString(a.VpcId) == vpc, aws.ToString(b.VpcId) == vpc
		if aMatch != bMatch {
			if aMatch {
				return -1
			}
			return 1
		}
		return strings.Compare(aws.ToString(a.InstanceId), aws.ToString(b.InstanceId))
	})
}

func instanceName(instance *ec2types.Instance) string {
	for _, tag := range instance.Tags {
		if aws.ToString(tag.Key) == "Name" {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}
