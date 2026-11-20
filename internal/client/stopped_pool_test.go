package client

import (
	"context"
	"encoding/base64"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStoppedClaimConcurrentOneUse(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	instance := types.Instance{InstanceId: aws.String("i-test"), ImageId: aws.String(s.BootstrapParams.Image), State: &types.InstanceState{Name: types.InstanceStateNameStopped}, Tags: []types.Tag{
		{Key: aws.String("GARM_CONTROLLER_ID"), Value: aws.String(s.ControllerID)},
		{Key: aws.String("BFD_STOPPED_POOL"), Value: aws.String(s.StoppedPool)},
		{Key: aws.String("BFD_STOPPED_PLAN"), Value: aws.String(s.StoppedPlan())},
	}}
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance}}}}, nil)
	modify := m.On("ModifyInstanceAttribute", mock.Anything, mock.MatchedBy(func(in *ec2.ModifyInstanceAttributeInput) bool { return string(in.UserData.Value) == "bootstrap" }), mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
	tags := m.On("CreateTags", mock.Anything, mock.MatchedBy(func(in *ec2.CreateTagsInput) bool {
		return tagValue(in.Tags, "GARM_POOL_ID") == s.BootstrapParams.PoolID && tagValue(in.Tags, "BFD_STOPPED_POOL") == "claimed"
	}), mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once().NotBefore(modify)
	m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.StartInstancesOutput{}, nil).Once().NotBefore(tags)
	var wg sync.WaitGroup
	var claimed atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := cli.claimStopped(context.Background(), s, base64.StdEncoding.EncodeToString([]byte("bootstrap")))
			require.NoError(t, err)
			if id != "" {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, claimed.Load())
	m.AssertExpectations(t)
	// A fresh client process sharing disk cannot reuse the stale EC2 response.
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	id, err := other.claimStopped(context.Background(), s, base64.StdEncoding.EncodeToString([]byte("bootstrap")))
	require.NoError(t, err)
	require.Empty(t, id)
}
func TestEmptyStoppedPoolFallsBack(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil)
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Once()
	id, err := cli.CreateRunningInstance(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "i-cold", id)
	m.AssertNotCalled(t, "StartInstances", mock.Anything, mock.Anything, mock.Anything)
}

func TestClaimRejectsInstanceAlreadyOwnedByGarm(t *testing.T) {
	cli, m := newCapacityCli(t)
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{{InstanceId: aws.String("i-used"), State: &types.InstanceState{Name: types.InstanceStateNameStopped}, Tags: []types.Tag{{Key: aws.String("GARM_POOL_ID"), Value: aws.String("old-job")}}}}}}}, nil)
	id, err := cli.claimStopped(context.Background(), s, base64.StdEncoding.EncodeToString([]byte("bootstrap")))
	require.NoError(t, err)
	require.Empty(t, id)
	m.AssertNotCalled(t, "ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything)
}
