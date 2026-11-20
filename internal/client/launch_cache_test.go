package client

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPlacementRetryer(t *testing.T) {
	r := placementRetryer{retry.NewStandard()}
	for _, code := range capacityErrors {
		require.False(t, r.IsErrorRetryable(&smithy.GenericAPIError{Code: code}))
	}
	require.True(t, r.IsErrorRetryable(&smithy.GenericAPIError{Code: "RequestLimitExceeded"}))
	require.False(t, r.IsErrorRetryable(&smithy.GenericAPIError{Code: "UnauthorizedOperation"}))
}
func TestCacheAcrossProviderProcesses(t *testing.T) {
	first, _ := newCapacityCli(t)
	first.cacheDir = t.TempDir()
	second, _ := newCapacityCli(t)
	second.cacheDir = first.cacheDir
	first.remember("image", "/dev/sda1", time.Hour)
	require.Equal(t, "/dev/sda1", second.cached("image"))
	first.remember("image", "expired", -time.Second)
	require.Empty(t, second.cached("image"))
	first.remember("image", "other region", time.Hour)
	second.cfg.Region = "us-west-2"
	require.Empty(t, second.cached("image"))
}
func TestRefusedPlacementLastWithinMarket(t *testing.T) {
	cli, _ := newCapacityCli(t)
	cli.cacheDir = t.TempDir()
	s := capacitySpec(true)
	original := s.LaunchAttempts()
	cli.remember(placementKey(s, original[0]), "refused", time.Minute)
	got := cli.launchAttempts(s)
	require.Equal(t, original[1], got[0])
	require.Equal(t, original[0], got[3])
	require.Equal(t, original[4:], got[4:])
	// A refusal never removes a placement, even when every one is refused.
	for _, p := range original {
		cli.remember(placementKey(s, p), "refused", time.Minute)
	}
	require.Equal(t, original, cli.launchAttempts(s))
}
func TestCachedImageSkipsDescribe(t *testing.T) {
	cli, m := newCapacityCli(t)
	cli.cacheDir = t.TempDir()
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-test")}}}, nil)
	_, err := cli.CreateRunningInstance(context.Background(), capacitySpec(false))
	require.NoError(t, err)
	_, err = cli.CreateRunningInstance(context.Background(), capacitySpec(false))
	require.NoError(t, err)
	m.AssertNumberOfCalls(t, "DescribeImages", 1)
}
