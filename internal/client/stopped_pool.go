package client

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/cloudbase/garm-provider-aws/internal/spec"
)

type stoppedClient interface {
	ModifyInstanceAttribute(context.Context, *ec2.ModifyInstanceAttributeInput, ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error)
	CreateTags(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

func tagValue(tags []types.Tag, key string) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

func (a *AwsCli) claimStopped(ctx context.Context, s *spec.RunnerSpec, userData string) (string, error) {
	if s.StoppedPool == "" || s.Spot {
		return "", nil
	}
	if a.cacheDir == "" {
		return "", fmt.Errorf("stopped pool requires writable cache_dir")
	}
	decoded, err := base64.StdEncoding.DecodeString(userData)
	if err != nil {
		return "", fmt.Errorf("decode stopped runner user data: %w", err)
	}
	cli, ok := a.client.(stoppedClient)
	if !ok {
		return "", fmt.Errorf("stopped pool client does not support claims")
	}
	claimDir := filepath.Join(a.cacheDir, "claims")
	if err := os.MkdirAll(claimDir, 0700); err != nil {
		return "", err
	}
	// The GARM controller is single-host. O_EXCL is the lock across its provider
	// processes; the durable file is a permanent one-use tombstone, even if EC2
	// returns stale tags to the next concurrent DescribeInstances call.
	response, err := a.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:GARM_CONTROLLER_ID"), Values: []string{s.ControllerID}},
		{Name: aws.String("tag:BFD_STOPPED_POOL"), Values: []string{s.StoppedPool}},
		{Name: aws.String("tag:BFD_STOPPED_PLAN"), Values: []string{s.StoppedPlan()}},
		{Name: aws.String("instance-state-name"), Values: []string{"stopped"}},
		{Name: aws.String("image-id"), Values: []string{s.BootstrapParams.Image}},
	}})
	if err != nil {
		return "", fmt.Errorf("list stopped pool: %w", err)
	}
	for _, reservation := range response.Reservations {
		for _, instance := range reservation.Instances {
			id := aws.ToString(instance.InstanceId)
			if id == "" || filepath.Base(id) != id || tagValue(instance.Tags, "GARM_POOL_ID") != "" || instance.State == nil || instance.State.Name != types.InstanceStateNameStopped {
				continue
			}
			// Never trust an old Describe response across claims, or a mismatched image.
			if aws.ToString(instance.ImageId) != s.BootstrapParams.Image || tagValue(instance.Tags, "BFD_STOPPED_PLAN") != s.StoppedPlan() || tagValue(instance.Tags, "BFD_STOPPED_POOL") != s.StoppedPool || tagValue(instance.Tags, "GARM_CONTROLLER_ID") != s.ControllerID {
				continue
			}
			f, err := os.OpenFile(filepath.Join(claimDir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if os.IsExist(err) {
				continue
			}
			if err != nil {
				return "", err
			}
			_, writeErr := fmt.Fprintln(f, s.BootstrapParams.Name)
			syncErr := f.Sync()
			defer f.Close()
			if err := lockClaim(f); err != nil {
				return "", err
			}
			if writeErr != nil || syncErr != nil {
				return "", fmt.Errorf("persist stopped instance claim")
			}
			// User data must be replaced while stopped, before publishing ownership to
			// GARM. A crash leaves a tombstone the keeper safely retires, never reuses.
			_, err = cli.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: aws.String(id), UserData: &types.SecureBlobAttributeValue{Value: decoded}})
			if err != nil {
				return "", fmt.Errorf("set stopped runner user data: %w", err)
			}
			tags := []types.Tag{
				{Key: aws.String("Name"), Value: aws.String(s.BootstrapParams.Name)},
				{Key: aws.String("GARM_POOL_ID"), Value: aws.String(s.BootstrapParams.PoolID)},
				{Key: aws.String("OSType"), Value: aws.String(string(s.BootstrapParams.OSType))},
				{Key: aws.String("OSArch"), Value: aws.String(string(s.BootstrapParams.OSArch))},
				{Key: aws.String("BFD_STOPPED_POOL"), Value: aws.String("claimed")},
			}
			_, err = cli.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: tags})
			if err != nil {
				return "", fmt.Errorf("tag stopped runner: %w", err)
			}
			_, err = a.client.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}})
			if err != nil {
				if !isCapacityError(err) {
					return "", fmt.Errorf("start claimed runner (outcome may be uncertain): %w", err)
				}
				// A definitive placement refusal permits a cold launch. Never retry a
				// possibly successful start by launching a second machine for the job.
				if stopErr := a.TerminateInstance(ctx, id); stopErr != nil {
					return "", stopErr
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "garm-provider-aws: started stopped pool instance %s at %s\n", id, time.Now().UTC().Format(time.RFC3339))
			return id, nil
		}
	}
	return "", nil
}
