package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(testColdInstance()), nil).Times(11)
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
			expectInitialClaimLookup(m, candidate)
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
				response = instanceResponse(types.Instance{InstanceId: aws.String("i-cold"), ClientToken: original.ClientToken, ImageId: original.ImageId, State: &types.InstanceState{Name: types.InstanceStateNamePending}, Tags: original.TagSpecifications[0].Tags})
			} else {
				m.On("RunInstances", mock.Anything, mock.MatchedBy(func(in *ec2.RunInstancesInput) bool {
					return aws.ToString(in.ClientToken) == aws.ToString(original.ClientToken) && aws.ToString(in.SubnetId) == aws.ToString(original.SubnetId) && in.InstanceType == original.InstanceType && aws.ToString(in.UserData) == aws.ToString(original.UserData)
				}), mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Once().NotBefore(first)
			}
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
				return len(in.InstanceIds) == 0 && len(in.Filters) > 0 && aws.ToString(in.Filters[0].Name) == "client-token" && in.Filters[0].Values[0] == aws.ToString(original.ClientToken)
			}), mock.Anything).Return(response, nil).Once()
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(testColdInstance()), nil).Once()
			// Capacity-cache ordering must not change an uncertain request.
			cli.remember(placementKey(s, s.LaunchAttempts()[0]), "refused", time.Minute)
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
			calls := 1
			if expired {
				calls = 2
			}
			m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Times(calls)
			m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.RunInstancesOutput)(nil), fmt.Errorf("response lost")).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			require.Error(t, err)
			if expired {
				files, err := filepath.Glob(filepath.Join(cli.cacheDir, "runner-claims", "*.lock"))
				require.NoError(t, err)
				require.Len(t, files, 1)
				path := files[0] + ".json"
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var claim runnerClaim
				require.NoError(t, json.Unmarshal(data, &claim))
				claim.Cold.Created = time.Now().Add(-13 * time.Hour)
				f, err := os.OpenFile(strings.TrimSuffix(path, ".json"), os.O_RDWR, 0600)
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
	candidate := types.Instance{InstanceId: aws.String("i-first"), ClientToken: original.ClientToken, ImageId: original.ImageId, State: &types.InstanceState{Name: types.InstanceStateNamePending}, Tags: original.TagSpecifications[0].Tags}
	second := candidate
	second.InstanceId = aws.String("i-second")
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return aws.ToString(in.Filters[0].Name) == "client-token" }), mock.Anything).Return(&ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{candidate}}, {Instances: []types.Instance{second}}}}, nil).Once()
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	_, err = other.CreateRunningInstance(context.Background(), s)
	require.ErrorContains(t, err, "multiple instances")
	m.AssertExpectations(t)
}

func testColdInstance() types.Instance {
	return types.Instance{InstanceId: aws.String("i-cold"), ImageId: aws.String("ami-12345678"), ClientToken: aws.String("ecab906ed5c03b755208eb2a18235c7cfb7c4a124d00c74879c0e63e92b82580"), State: &types.InstanceState{Name: types.InstanceStateNameRunning}, Tags: []types.Tag{
		{Key: aws.String("GARM_CONTROLLER_ID"), Value: aws.String("controller")},
		{Key: aws.String("GARM_POOL_ID"), Value: aws.String("pool")},
		{Key: aws.String("Name"), Value: aws.String("runner")},
	}}
}

func TestEveryColdRecoveryPathRejectsChangedParameters(t *testing.T) {
	for _, saved := range []bool{false, true} {
		for _, change := range []string{"image", "profile", "bootstrap", "placements"} {
			t.Run(fmt.Sprintf("saved=%t/%s", saved, change), func(t *testing.T) {
				cli, m := newCapacityCli(t)
				m.ExpectedCalls[0].Maybe()
				cli.cacheDir = t.TempDir()
				s := capacitySpec(false)
				s.StoppedPool = "bfd-linux"
				m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
				var runErr error
				var output *ec2.RunInstancesOutput
				if saved {
					output = &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}
				} else {
					runErr = fmt.Errorf("lost response")
				}
				m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(output, runErr).Once()
				_, err := cli.CreateRunningInstance(context.Background(), s)
				if saved {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				switch change {
				case "image":
					s.BootstrapParams.Image = "ami-new"
				case "profile":
					s.IAMInstanceProfile = &types.IamInstanceProfileSpecification{Name: aws.String("different-profile")}
				case "bootstrap":
					s.BootstrapParams.InstanceToken = "new-token"
				case "placements":
					s.FallbackSubnets = append(s.FallbackSubnets, "subnet-cccccccc")
				}
				other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
				_, err = other.CreateRunningInstance(context.Background(), s)
				require.ErrorContains(t, err, "parameters changed")
				// No token lookup or saved-ID fast return may bypass the request check.
				m.AssertExpectations(t)
			})
		}
	}
}

func TestColdRecoveryRechecksLiveOwnershipAndImage(t *testing.T) {
	for _, saved := range []bool{false, true} {
		for _, change := range []string{"pool", "name", "controller", "image", "missing"} {
			if !saved && change == "missing" {
				continue
			} // Unknown requests may replay.
			t.Run(fmt.Sprintf("saved=%t/%s", saved, change), func(t *testing.T) {
				cli, m := newCapacityCli(t)
				m.ExpectedCalls[0].Maybe()
				cli.cacheDir = t.TempDir()
				s := capacitySpec(false)
				s.StoppedPool = "bfd-linux"
				m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
					return len(in.Filters) > 0 && aws.ToString(in.Filters[0].Name) == "tag:GARM_CONTROLLER_ID"
				}), mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
				var runErr error
				var output *ec2.RunInstancesOutput
				if saved {
					output = &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}
				} else {
					runErr = fmt.Errorf("lost response")
				}
				m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(output, runErr).Once()
				_, err := cli.CreateRunningInstance(context.Background(), s)
				if saved {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				instance := testColdInstance()
				switch change {
				case "pool":
					instance.Tags[1].Value = aws.String("other-pool")
				case "name":
					instance.Tags[2].Value = aws.String("other-runner")
				case "controller":
					instance.Tags[0].Value = aws.String("other-controller")
				case "image":
					instance.ImageId = aws.String("ami-other")
				}
				response := instanceResponse(instance)
				if change == "missing" {
					response = &ec2.DescribeInstancesOutput{}
				}
				m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
					return len(in.Filters) > 0 && aws.ToString(in.Filters[0].Name) == "client-token"
				}), mock.Anything).Return(response, nil).Once()
				other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
				_, err = other.CreateRunningInstance(context.Background(), s)
				require.Error(t, err)
				m.AssertExpectations(t)
			})
		}
	}
}

func TestColdRecoveryDoesNotDependOnImageLookup(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprint(saved), func(t *testing.T) {
			cli, m := newCapacityCli(t)
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			m.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
			var output *ec2.RunInstancesOutput
			var runErr error
			if saved {
				output = &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}
			} else {
				runErr = fmt.Errorf("lost response")
			}
			m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(output, runErr).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			if saved {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, os.Remove(cli.cachePath("image/"+s.ControllerID+"/"+s.BootstrapParams.Image)))
			next := new(MockComputeClient)
			next.On("DescribeInstances", mock.Anything, mock.Anything, mock.Anything).Return(instanceResponse(testColdInstance()), nil).Once()
			next.On("DescribeImages", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.DescribeImagesOutput)(nil), fmt.Errorf("AMI deregistered or EC2 lookup unavailable")).Maybe()
			other := &AwsCli{cfg: cli.cfg, client: next, cacheDir: cli.cacheDir}
			id, err := other.CreateRunningInstance(context.Background(), s)
			require.NoError(t, err)
			require.Equal(t, "i-cold", id)
			next.AssertNotCalled(t, "DescribeImages", mock.Anything, mock.Anything, mock.Anything)
			next.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
			next.AssertExpectations(t)
		})
	}
}
