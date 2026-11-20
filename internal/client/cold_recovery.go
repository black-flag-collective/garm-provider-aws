package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/cloudbase/garm-provider-aws/internal/spec"
)

// No bootstrap credentials are persisted: only a request digest, placement,
// idempotency token and resulting instance ID. The per-runner lock covers both
// the reserve and cold-launch paths.
type coldClaim struct {
	Attempt spec.LaunchAttempt `json:"attempt"`
	Token   string             `json:"token"`
	Digest  string             `json:"digest"`
	Plan    string             `json:"plan"`
	Created time.Time          `json:"created"`
	ID      string             `json:"id,omitempty"`
}

func prepareCold(record *os.File, claim *runnerClaim, s *spec.RunnerSpec, attempt spec.LaunchAttempt, input *ec2.RunInstancesInput) error {
	key := fmt.Sprintf("%s/%s/%s/%t/%s/%s", s.ControllerID, s.BootstrapParams.PoolID, s.BootstrapParams.Name, attempt.Spot, attempt.Flavor, attempt.SubnetID)
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	input.ClientToken = aws.String(token)
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	if claim.Cold != nil {
		if claim.Cold.Token != token || claim.Cold.Digest != digest || claim.Cold.Plan != s.StoppedPlan() {
			return fmt.Errorf("cold launch parameters changed from the recorded request")
		}
		return nil
	}
	claim.Cold = &coldClaim{Attempt: attempt, Token: token, Digest: digest, Plan: s.StoppedPlan(), Created: time.Now().UTC()}
	return saveRunnerClaim(record, *claim)
}

func (a *AwsCli) recoverCold(ctx context.Context, s *spec.RunnerSpec, record *os.File, claim *runnerClaim) (string, error) {
	input := &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("client-token"), Values: []string{claim.Cold.Token}},
		{Name: aws.String("tag:GARM_CONTROLLER_ID"), Values: []string{s.ControllerID}},
	}}
	if claim.Cold.ID != "" {
		input.InstanceIds = []string{claim.Cold.ID}
	}
	response, err := a.client.DescribeInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("recover cold runner: %w", err)
	}
	var matches []types.Instance
	for _, reservation := range response.Reservations {
		matches = append(matches, reservation.Instances...)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple instances match the cold launch token; refusing ambiguous recovery")
	}
	if len(matches) == 1 {
		instance := matches[0]
		if aws.ToString(instance.ClientToken) != claim.Cold.Token || tagValue(instance.Tags, "Name") != s.BootstrapParams.Name || tagValue(instance.Tags, "GARM_POOL_ID") != s.BootstrapParams.PoolID || tagValue(instance.Tags, "GARM_CONTROLLER_ID") != s.ControllerID {
			return "", fmt.Errorf("cold runner ownership not yet visible")
		}
		if aws.ToString(instance.ImageId) != s.BootstrapParams.Image {
			return "", fmt.Errorf("cold runner does not match requested image")
		}
		if claim.Cold.ID != "" && aws.ToString(instance.InstanceId) != claim.Cold.ID {
			return "", fmt.Errorf("cold runner ID mismatch")
		}
		if instance.State == nil || (instance.State.Name != types.InstanceStateNamePending && instance.State.Name != types.InstanceStateNameRunning) {
			return "", fmt.Errorf("cold runner is no longer starting or running")
		}
		id := aws.ToString(instance.InstanceId)
		if id == "" {
			return "", fmt.Errorf("cold runner ID not yet visible")
		}
		claim.Cold.ID = id
		if err := saveRunnerClaim(record, *claim); err != nil {
			return "", err
		}
		return id, nil
	}
	if claim.Cold.ID != "" {
		return "", fmt.Errorf("recorded cold runner not yet visible")
	}
	// EC2 idempotency tokens have a bounded lifetime. Never replay an old unknown
	// result after the retention window; a later Describe can still recover it.
	if time.Since(claim.Cold.Created) > 12*time.Hour {
		return "", fmt.Errorf("cold launch outcome still unknown; refusing an expired token replay")
	}
	return "", nil
}
