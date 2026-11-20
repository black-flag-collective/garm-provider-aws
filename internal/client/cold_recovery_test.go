package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestConcurrentEmptyPoolCreatesLaunchOneColdInstance(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
	m.On("RunInstances", mock.Anything, mock.MatchedBy(func(in *ec2.RunInstancesInput) bool { return len(aws.ToString(in.ClientToken)) == 64 }), mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Once()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			id, err := other.CreateRunningInstance(context.Background(), s)
			require.NoError(t, err)
			require.Equal(t, "i-cold", id)
		}()
	}
	wg.Wait()
	m.AssertExpectations(t)
}

func TestUncertainColdFallbackRecoversOrReplaysSamePlacement(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(fmt.Sprint(visible), func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
				return len(in.Filters) > 0 && aws.ToString(in.Filters[0].Name) == "tag:GARM_CONTROLLER_ID"
			}), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
			m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
			m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.StartInstancesOutput)(nil), noCapacity).Once()
			m.On("TerminateInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.TerminateInstancesOutput{}, nil).Once()
			var original *ec2.RunInstancesInput
			first := m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) { original = args.Get(1).(*ec2.RunInstancesInput) }).Return((*ec2.RunInstancesOutput)(nil), fmt.Errorf("response lost")).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			require.ErrorContains(t, err, "response lost")
			require.NotEmpty(t, aws.ToString(original.ClientToken))
			response := &ec2.DescribeInstancesOutput{}
			if visible {
				response = instanceResponse(types.Instance{InstanceId: aws.String("i-cold"), ClientToken: original.ClientToken, State: &types.InstanceState{Name: types.InstanceStateNamePending}, Tags: original.TagSpecifications[0].Tags})
				// Updated bootstrap credentials do not prevent recovering an already
				// accepted launch by its persisted token.
				s.BootstrapParams.InstanceToken = "updated-jit-secret"
			} else {
				m.On("RunInstances", mock.Anything, mock.MatchedBy(func(in *ec2.RunInstancesInput) bool {
					return aws.ToString(in.ClientToken) == aws.ToString(original.ClientToken) && aws.ToString(in.SubnetId) == aws.ToString(original.SubnetId) && in.InstanceType == original.InstanceType && aws.ToString(in.UserData) == aws.ToString(original.UserData)
				}), mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Once().NotBefore(first)
			}
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
				return len(in.Filters) > 0 && aws.ToString(in.Filters[0].Name) == "client-token" && in.Filters[0].Values[0] == aws.ToString(original.ClientToken)
			}), mock.Anything).Return(response, nil).Once()
			// A newly preferred subnet must not change an uncertain request's placement.
			s.SubnetID, s.FallbackSubnets[0] = s.FallbackSubnets[0], s.SubnetID
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			id, err := other.CreateRunningInstance(context.Background(), s)
			require.NoError(t, err)
			require.Equal(t, "i-cold", id)
			id, err = other.CreateRunningInstance(context.Background(), s)
			require.NoError(t, err)
			require.Equal(t, "i-cold", id)
			files, err := os.ReadDir(filepath.Join(cli.cacheDir, "runner-claims"))
			require.NoError(t, err)
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(cli.cacheDir, "runner-claims", file.Name()))
				require.NoError(t, err)
				require.NotContains(t, string(data), "updated-jit-secret")
				require.NotContains(t, string(data), aws.ToString(original.UserData))
			}
			m.AssertExpectations(t)
		})
	}
}

func TestUnknownColdOutcomeNeverChangesRequestOrReplaysExpiredToken(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Twice()
			m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.RunInstancesOutput)(nil), fmt.Errorf("response lost")).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			require.Error(t, err)
			if expired {
				files, err := os.ReadDir(filepath.Join(cli.cacheDir, "runner-claims"))
				require.NoError(t, err)
				require.Len(t, files, 1)
				path := filepath.Join(cli.cacheDir, "runner-claims", files[0].Name())
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var claim runnerClaim
				require.NoError(t, json.Unmarshal(data, &claim))
				claim.Cold.Created = time.Now().Add(-13 * time.Hour)
				f, err := os.OpenFile(path, os.O_RDWR, 0600)
				require.NoError(t, err)
				require.NoError(t, saveRunnerClaim(f, claim))
				require.NoError(t, f.Close())
			} else {
				s.BootstrapParams.InstanceToken = "changed-bootstrap-token"
			}
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			_, err = other.CreateRunningInstance(context.Background(), s)
			if expired {
				require.ErrorContains(t, err, "expired token replay")
			} else {
				require.ErrorContains(t, err, "parameters changed")
			}
			m.AssertExpectations(t)
		})
	}
}

func TestColdRecoveryRejectsMultipleTokenMatches(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
		return aws.ToString(in.Filters[0].Name) == "tag:GARM_CONTROLLER_ID"
	}), mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
	var original *ec2.RunInstancesInput
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) { original = args.Get(1).(*ec2.RunInstancesInput) }).Return((*ec2.RunInstancesOutput)(nil), fmt.Errorf("response lost")).Once()
	_, err := cli.CreateRunningInstance(context.Background(), s)
	require.Error(t, err)
	candidate := types.Instance{InstanceId: aws.String("i-first"), ClientToken: original.ClientToken, State: &types.InstanceState{Name: types.InstanceStateNamePending}, Tags: original.TagSpecifications[0].Tags}
	second := candidate
	second.InstanceId = aws.String("i-second")
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return aws.ToString(in.Filters[0].Name) == "client-token" }), mock.Anything).Return(&ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{candidate}}, {Instances: []types.Instance{second}}}}, nil).Once()
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	_, err = other.CreateRunningInstance(context.Background(), s)
	require.ErrorContains(t, err, "multiple instances")
	m.AssertExpectations(t)
}
