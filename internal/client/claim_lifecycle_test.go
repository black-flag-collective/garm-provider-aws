package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func readRunnerClaim(t *testing.T, dir string) runnerClaim {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "runner-claims", "*.json"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	data, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	var claim runnerClaim
	require.NoError(t, json.Unmarshal(data, &claim))
	return claim
}

func TestDeleteRetiresOnlyMatchingRunnerAndRotatesColdToken(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Twice()
	first := m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-old")}}}, nil).Once()
	id, err := cli.CreateRunningInstance(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "i-old", id)
	old := readRunnerClaim(t, cli.cacheDir)
	require.NotEmpty(t, old.Cold.Token)
	failure := m.On("TerminateInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.TerminateInstancesOutput)(nil), fmt.Errorf("uncertain termination")).Once()
	require.Error(t, cli.TerminateInstance(context.Background(), "i-old"))
	require.Equal(t, old, readRunnerClaim(t, cli.cacheDir))
	m.On("TerminateInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.TerminateInstancesOutput{}, nil).Twice().NotBefore(failure)
	require.NoError(t, cli.TerminateInstance(context.Background(), "i-old"))
	retired := readRunnerClaim(t, cli.cacheDir)
	require.Empty(t, retired.ID)
	require.Nil(t, retired.Cold)
	require.NotEmpty(t, retired.Generation)
	m.On("RunInstances", mock.Anything, mock.MatchedBy(func(in *ec2.RunInstancesInput) bool {
		return aws.ToString(in.ClientToken) != old.Cold.Token && len(aws.ToString(in.ClientToken)) == 64
	}), mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-new")}}}, nil).Once().NotBefore(first)
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	id, err = other.CreateRunningInstance(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "i-new", id)
	newer := readRunnerClaim(t, cli.cacheDir)
	require.NoError(t, other.TerminateInstance(context.Background(), "i-old"))
	require.Equal(t, newer, readRunnerClaim(t, cli.cacheDir), "late delete must not retire the replacement")
	m.AssertExpectations(t)
}

func TestDeletingPoolRunnerKeepsOneUseTombstone(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(instanceResponse(candidate), nil).Twice()
	m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
	m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
	m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.StartInstancesOutput{}, nil).Once()
	id, err := cli.CreateRunningInstance(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "i-claim", id)
	m.On("TerminateInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.TerminateInstancesOutput{}, nil).Once()
	require.NoError(t, cli.TerminateInstance(context.Background(), id))
	retired := readRunnerClaim(t, cli.cacheDir)
	require.Empty(t, retired.ID)
	require.NotEmpty(t, retired.Generation)
	_, err = os.Stat(filepath.Join(cli.cacheDir, "claims", id))
	require.NoError(t, err)
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(instanceResponse(candidate), nil).Once()
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-replacement")}}}, nil).Once()
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	id, err = other.CreateRunningInstance(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "i-replacement", id)
	m.AssertExpectations(t)
}
