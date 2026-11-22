// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/cloudbase/garm-provider-aws/config"
	"github.com/cloudbase/garm-provider-aws/internal/spec"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func capacitySpec(spot bool) *spec.RunnerSpec {
	return &spec.RunnerSpec{
		Region: "us-east-1",
		Tools: params.RunnerApplicationDownload{
			OS: aws.String("linux"), Architecture: aws.String("amd64"),
			DownloadURL: aws.String("MockURL"), Filename: aws.String("garm-runner"),
		},
		BootstrapParams: params.BootstrapInstance{
			Name: "runner", OSType: "linux", Image: "ami-12345678", Flavor: "m8a.large", PoolID: "pool",
		},
		SubnetID:        "subnet-aaaaaaaa",
		FallbackSubnets: []string{"subnet-bbbbbbbb"},
		FallbackFlavors: []string{"m7a.large"},
		Spot:            spot,
		ControllerID:    "controller",
	}
}

func newCapacityCli(t *testing.T) (*AwsCli, *MockComputeClient) {
	t.Helper()
	m := new(MockComputeClient)
	t.Cleanup(func() {
		for _, call := range m.Calls {
			if call.Method == "RunInstances" {
				assertLaunchCostTags(t, call.Arguments.Get(1).(*ec2.RunInstancesInput), "runner", "pool", "controller", "linux", "")
			}
		}
	})
	m.On("DescribeImages", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeImagesOutput{
		Images: []types.Image{{ImageId: aws.String("ami-12345678"), RootDeviceName: aws.String("/dev/sda1")}},
	}, nil)
	return &AwsCli{cfg: &config.Config{Region: "us-east-1"}, client: m}, m
}

func placement(spot bool, flavor, subnet string) func(*ec2.RunInstancesInput) bool {
	return func(in *ec2.RunInstancesInput) bool {
		isSpot := in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == types.MarketTypeSpot
		return isSpot == spot && string(in.InstanceType) == flavor && aws.ToString(in.SubnetId) == subnet
	}
}

var noCapacity = &smithy.GenericAPIError{Code: "InsufficientInstanceCapacity", Message: "none left"}

func TestLaunchAttemptsOrder(t *testing.T) {
	got := capacitySpec(true).LaunchAttempts()
	want := []spec.LaunchAttempt{
		{Spot: true, Flavor: "m8a.large", SubnetID: "subnet-aaaaaaaa"},
		{Spot: true, Flavor: "m8a.large", SubnetID: "subnet-bbbbbbbb"},
		{Spot: true, Flavor: "m7a.large", SubnetID: "subnet-aaaaaaaa"},
		{Spot: true, Flavor: "m7a.large", SubnetID: "subnet-bbbbbbbb"},
		{Spot: false, Flavor: "m8a.large", SubnetID: "subnet-aaaaaaaa"},
		{Spot: false, Flavor: "m8a.large", SubnetID: "subnet-bbbbbbbb"},
		{Spot: false, Flavor: "m7a.large", SubnetID: "subnet-aaaaaaaa"},
		{Spot: false, Flavor: "m7a.large", SubnetID: "subnet-bbbbbbbb"},
	}
	require.Equal(t, want, got)

	s := capacitySpec(true)
	s.SpotOnly = true
	require.Len(t, s.LaunchAttempts(), 4)

	require.Len(t, capacitySpec(false).LaunchAttempts(), 4)

	plain := &spec.RunnerSpec{BootstrapParams: params.BootstrapInstance{Flavor: "m8a.large"}, SubnetID: "subnet-aaaaaaaa"}
	require.Equal(t, []spec.LaunchAttempt{{Flavor: "m8a.large", SubnetID: "subnet-aaaaaaaa"}}, plain.LaunchAttempts())
}

func TestCreateRunningInstanceFallsBackOnCapacity(t *testing.T) {
	ctx := context.Background()
	cli, m := newCapacityCli(t)
	m.On("RunInstances", ctx, mock.MatchedBy(placement(true, "m8a.large", "subnet-aaaaaaaa")), mock.Anything).
		Return((*ec2.RunInstancesOutput)(nil), noCapacity).Once()
	m.On("RunInstances", ctx, mock.MatchedBy(placement(true, "m8a.large", "subnet-bbbbbbbb")), mock.Anything).
		Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-spotb")}}}, nil).Once()

	id, err := cli.CreateRunningInstance(ctx, capacitySpec(true))
	require.NoError(t, err)
	require.Equal(t, "i-spotb", id)
	m.AssertNumberOfCalls(t, "RunInstances", 2)
}

func TestCreateRunningInstanceUsesOnDemandWhenSpotIsGone(t *testing.T) {
	ctx := context.Background()
	cli, m := newCapacityCli(t)
	m.On("RunInstances", ctx, mock.MatchedBy(func(in *ec2.RunInstancesInput) bool { return in.InstanceMarketOptions != nil }), mock.Anything).
		Return((*ec2.RunInstancesOutput)(nil), noCapacity)
	m.On("RunInstances", ctx, mock.MatchedBy(placement(false, "m8a.large", "subnet-aaaaaaaa")), mock.Anything).
		Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-ondemand")}}}, nil).Once()

	id, err := cli.CreateRunningInstance(ctx, capacitySpec(true))
	require.NoError(t, err)
	require.Equal(t, "i-ondemand", id)
	m.AssertNumberOfCalls(t, "RunInstances", 5)
}

func TestCreateRunningInstanceStopsOnOtherErrors(t *testing.T) {
	ctx := context.Background()
	cli, m := newCapacityCli(t)
	denied := &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "no"}
	m.On("RunInstances", ctx, mock.Anything, mock.Anything).Return((*ec2.RunInstancesOutput)(nil), denied).Once()

	_, err := cli.CreateRunningInstance(ctx, capacitySpec(true))
	require.Error(t, err)
	require.True(t, errors.As(err, new(smithy.APIError)))
	m.AssertNumberOfCalls(t, "RunInstances", 1)
}

func TestCreateRunningInstanceReportsWhenEveryPlacementIsFull(t *testing.T) {
	ctx := context.Background()
	cli, m := newCapacityCli(t)
	m.On("RunInstances", ctx, mock.Anything, mock.Anything).Return((*ec2.RunInstancesOutput)(nil), noCapacity)

	_, err := cli.CreateRunningInstance(ctx, capacitySpec(true))
	require.ErrorContains(t, err, "no capacity in any placement")
	m.AssertNumberOfCalls(t, "RunInstances", 8)
}

func TestSubnetAddressExhaustionFallsBack(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.On("RunInstances", mock.Anything, mock.MatchedBy(placement(false, "m8a.large", "subnet-aaaaaaaa")), mock.Anything).Return((*ec2.RunInstancesOutput)(nil), &smithy.GenericAPIError{Code: "InsufficientFreeAddressesInSubnet"}).Once()
	m.On("RunInstances", mock.Anything, mock.MatchedBy(placement(false, "m8a.large", "subnet-bbbbbbbb")), mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-next")}}}, nil).Once()
	id, err := cli.CreateRunningInstance(context.Background(), capacitySpec(false))
	require.NoError(t, err)
	require.Equal(t, "i-next", id)
	m.AssertExpectations(t)
}
func TestUnsupportedRequiresPlacementSpecificMessage(t *testing.T) {
	require.False(t, isCapacityError(&smithy.GenericAPIError{Code: "Unsupported", Message: "The requested configuration is not supported"}))
	placementErr := &smithy.GenericAPIError{Code: "Unsupported", Message: "Your requested instance type (m8a.large) is not supported in your requested Availability Zone (us-east-1a)."}
	require.True(t, isCapacityError(placementErr))
	cli, m := newCapacityCli(t)
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.RunInstancesOutput)(nil), &smithy.GenericAPIError{Code: "Unsupported", Message: "Unsupported configuration"}).Once()
	_, err := cli.CreateRunningInstance(context.Background(), capacitySpec(false))
	require.Error(t, err)
	m.AssertNumberOfCalls(t, "RunInstances", 1)
}
