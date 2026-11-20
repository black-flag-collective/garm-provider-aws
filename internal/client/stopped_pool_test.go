package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"path/filepath"
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
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Times(12)
	var wg sync.WaitGroup
	var claimed atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			own := *s
			own.BootstrapParams.Name = fmt.Sprintf("runner-%d", i)
			id, err := cli.claimStopped(context.Background(), &own, base64.StdEncoding.EncodeToString([]byte("bootstrap")))
			require.NoError(t, err)
			if id == "i-test" {
				claimed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 1, claimed.Load())
	// A fresh client process sharing disk cannot reuse the stale EC2 response.
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	id, err := other.claimStopped(context.Background(), s, base64.StdEncoding.EncodeToString([]byte("bootstrap")))
	require.NoError(t, err)
	require.Equal(t, "i-cold", id)
	m.AssertExpectations(t)
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
	m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Once()
	id, err := cli.claimStopped(context.Background(), s, base64.StdEncoding.EncodeToString([]byte("bootstrap")))
	require.NoError(t, err)
	require.Equal(t, "i-cold", id)
	m.AssertNotCalled(t, "ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything)
}

func TestStartFailureOnlyFallsBackWhenOutcomeIsDefinitive(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			candidate := types.Instance{InstanceId: aws.String("i-claim"), ImageId: aws.String(s.BootstrapParams.Image), State: &types.InstanceState{Name: types.InstanceStateNameStopped}, Tags: []types.Tag{
				{Key: aws.String("GARM_CONTROLLER_ID"), Value: aws.String(s.ControllerID)},
				{Key: aws.String("BFD_STOPPED_POOL"), Value: aws.String(s.StoppedPool)},
				{Key: aws.String("BFD_STOPPED_PLAN"), Value: aws.String(s.StoppedPlan())},
			}}
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(&ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{candidate}}}}, nil)
			expectInitialClaimLookup(m, candidate)
			m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
			m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
			var startErr error = fmt.Errorf("transport timeout: outcome unknown")
			if capacity {
				startErr = noCapacity
				m.On("TerminateInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.TerminateInstancesOutput{}, nil).Once()
				m.On("RunInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-cold")}}}, nil).Once()
			}
			m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.StartInstancesOutput)(nil), startErr).Once()
			id, err := cli.CreateRunningInstance(context.Background(), s)
			if capacity {
				require.NoError(t, err)
				require.Equal(t, "i-cold", id)
				m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(testColdInstance()), nil).Once()
				other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
				recovered, err := other.CreateRunningInstance(context.Background(), s)
				require.NoError(t, err)
				require.Equal(t, id, recovered)
			} else {
				require.ErrorContains(t, err, "outcome may be uncertain")
				require.Empty(t, id)
				m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
				m.AssertNotCalled(t, "TerminateInstances", mock.Anything, mock.Anything, mock.Anything)
			}
			m.AssertExpectations(t)
		})
	}
}

func TestSameRunnerConcurrentCreatesResumeOneClaim(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
	expectInitialClaimLookup(m, candidate)
	running := candidate
	running.State = &types.InstanceState{Name: types.InstanceStateNameRunning}
	running.Tags = append(append([]types.Tag{}, candidate.Tags...), types.Tag{Key: aws.String("Name"), Value: aws.String(s.BootstrapParams.Name)}, types.Tag{Key: aws.String("GARM_POOL_ID"), Value: aws.String(s.BootstrapParams.PoolID)})
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
		return len(in.InstanceIds) == 1 && in.InstanceIds[0] == "i-claim"
	}), mock.Anything).Return(instanceResponse(running), nil).Times(11)
	entered, release := make(chan struct{}), make(chan struct{})
	m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Run(func(mock.Arguments) { close(entered); <-release }).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
	m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
	m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.StartInstancesOutput{}, nil).Once()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			id, err := other.CreateRunningInstance(context.Background(), s)
			require.NoError(t, err)
			require.Equal(t, "i-claim", id)
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
	m.AssertExpectations(t)
}

func poolCandidate(controller, pool, plan, image string) types.Instance {
	return types.Instance{InstanceId: aws.String("i-claim"), ImageId: aws.String(image), State: &types.InstanceState{Name: types.InstanceStateNameStopped}, Tags: []types.Tag{
		{Key: aws.String("GARM_CONTROLLER_ID"), Value: aws.String(controller)},
		{Key: aws.String("BFD_STOPPED_POOL"), Value: aws.String(pool)},
		{Key: aws.String("BFD_STOPPED_PLAN"), Value: aws.String(plan)},
	}}
}
func instanceResponse(instance types.Instance) *ec2.DescribeInstancesOutput {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance}}}}
}

func TestAmbiguousStartRetryRecoversActualInstance(t *testing.T) {
	for _, state := range []types.InstanceStateName{types.InstanceStateNamePending, types.InstanceStateNameRunning, types.InstanceStateNameStopped} {
		t.Run(string(state), func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			expectInitialClaimLookup(m, candidate)
			m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
			m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
			first := m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.StartInstancesOutput)(nil), fmt.Errorf("lost start response")).Once()
			id, err := cli.CreateRunningInstance(context.Background(), s)
			require.ErrorContains(t, err, "outcome may be uncertain")
			require.Empty(t, id)
			candidate.State.Name = state
			candidate.Tags = append(candidate.Tags, types.Tag{Key: aws.String("Name"), Value: aws.String(s.BootstrapParams.Name)}, types.Tag{Key: aws.String("GARM_POOL_ID"), Value: aws.String(s.BootstrapParams.PoolID)})
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
				return len(in.InstanceIds) == 1 && in.InstanceIds[0] == "i-claim"
			}), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			if state == types.InstanceStateNameStopped {
				m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.StartInstancesOutput{}, nil).Once().NotBefore(first)
			}
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			id, err = other.CreateRunningInstance(context.Background(), s)
			require.NoError(t, err)
			require.Equal(t, "i-claim", id)
			m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
			m.AssertExpectations(t)
		})
	}
}

func TestReservationPublishesWholeOwnerAndRejectsOtherRunner(t *testing.T) {
	dir := t.TempDir()
	f, err := reserveStopped(dir, "i-claim", "runner-a")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	data, err := os.ReadFile(filepath.Join(dir, "i-claim"))
	require.NoError(t, err)
	require.Equal(t, "runner-a\n", string(data))
	f, err = reserveStopped(dir, "i-claim", "runner-a")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = reserveStopped(dir, "i-claim", "runner-b")
	require.ErrorIs(t, err, os.ErrExist)
}

func TestLostTagResponseResumesWithoutRewritingClaimedSpare(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
	expectInitialClaimLookup(m, candidate)
	m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
	m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.CreateTagsOutput)(nil), fmt.Errorf("lost tag response")).Once()
	_, err := cli.CreateRunningInstance(context.Background(), s)
	require.ErrorContains(t, err, "tag stopped runner")
	candidate.Tags = append(candidate.Tags, types.Tag{Key: aws.String("Name"), Value: aws.String(s.BootstrapParams.Name)}, types.Tag{Key: aws.String("GARM_POOL_ID"), Value: aws.String(s.BootstrapParams.PoolID)})
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
	m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.StartInstancesOutput{}, nil).Once()
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	id, err := other.CreateRunningInstance(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "i-claim", id)
	m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
	m.AssertExpectations(t)
}

func TestUnknownClaimStateNeverFallsBack(t *testing.T) {
	cli, m := newCapacityCli(t)
	m.ExpectedCalls[0].Maybe()
	cli.cacheDir = t.TempDir()
	s := capacitySpec(false)
	s.StoppedPool = "bfd-linux"
	candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
	expectInitialClaimLookup(m, candidate)
	m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
	m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
	m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.StartInstancesOutput)(nil), fmt.Errorf("lost response")).Once()
	_, err := cli.CreateRunningInstance(context.Background(), s)
	require.Error(t, err)
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(&ec2.DescribeInstancesOutput{}, nil).Once()
	other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
	_, err = other.CreateRunningInstance(context.Background(), s)
	require.ErrorContains(t, err, "not yet visible")
	m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
	m.AssertExpectations(t)
}

func TestResumedClaimRejectsChangedPlan(t *testing.T) {
	for _, change := range []string{"request", "image", "tag"} {
		t.Run(change, func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			expectInitialClaimLookup(m, candidate)
			m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
			m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
			m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.StartInstancesOutput)(nil), fmt.Errorf("lost response")).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			require.Error(t, err)
			switch change {
			case "request":
				s.BootstrapParams.Image = "ami-new"
			case "image":
				candidate.ImageId = aws.String("ami-other")
			case "tag":
				candidate.Tags[2].Value = aws.String("other-plan")
			}
			if change != "request" {
				m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			}
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			_, err = other.CreateRunningInstance(context.Background(), s)
			require.ErrorContains(t, err, "does not match requested plan")
			m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
			m.AssertExpectations(t)
		})
	}
}

func TestClaimStateUsesAtomicReplacementAndStableLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_RDWR|os.O_CREATE, 0600)
	require.NoError(t, err)
	defer lock.Close()
	require.NoError(t, lockClaim(lock))
	before, err := lock.Stat()
	require.NoError(t, err)
	first := runnerClaim{ID: "i-first", Plan: "plan"}
	require.NoError(t, saveRunnerClaim(lock, first))
	snapshot, err := os.Open(lock.Name() + ".json")
	require.NoError(t, err)
	defer snapshot.Close()
	firstInfo, err := snapshot.Stat()
	require.NoError(t, err)
	require.NoError(t, saveRunnerClaim(lock, runnerClaim{ID: "i-second", Plan: "plan", Starting: true}))
	oldData, err := io.ReadAll(snapshot)
	require.NoError(t, err)
	var old runnerClaim
	require.NoError(t, json.Unmarshal(oldData, &old))
	require.Equal(t, first, old)
	data, err := os.ReadFile(lock.Name() + ".json")
	require.NoError(t, err)
	var current runnerClaim
	require.NoError(t, json.Unmarshal(data, &current))
	require.Equal(t, "i-second", current.ID)
	require.True(t, current.Starting)
	after, err := os.Stat(lock.Name())
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	stateInfo, err := os.Stat(lock.Name() + ".json")
	require.NoError(t, err)
	require.False(t, os.SameFile(firstInfo, stateInfo))
	require.Equal(t, os.FileMode(0600), stateInfo.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
}

func TestResumeOwnershipChecksApplyBeforeEveryStateAction(t *testing.T) {
	for _, state := range []types.InstanceStateName{types.InstanceStateNameStopped, types.InstanceStateNamePending, types.InstanceStateNameRunning, types.InstanceStateNameTerminated} {
		for _, changed := range []string{"pool", "name"} {
			t.Run(string(state)+"/"+changed, func(t *testing.T) {
				cli, m := newCapacityCli(t)
				m.ExpectedCalls[0].Maybe()
				cli.cacheDir = t.TempDir()
				s := capacitySpec(false)
				s.StoppedPool = "bfd-linux"
				candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
				m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
				expectInitialClaimLookup(m, candidate)
				m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once()
				m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
				m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.StartInstancesOutput)(nil), fmt.Errorf("lost response")).Once()
				_, err := cli.CreateRunningInstance(context.Background(), s)
				require.Error(t, err)
				pool, name := s.BootstrapParams.PoolID, s.BootstrapParams.Name
				if changed == "pool" {
					pool = "another-pool"
				} else {
					name = "another-runner"
				}
				candidate.State = &types.InstanceState{Name: state}
				candidate.Tags = append(candidate.Tags, types.Tag{Key: aws.String("GARM_POOL_ID"), Value: aws.String(pool)}, types.Tag{Key: aws.String("Name"), Value: aws.String(name)})
				m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
				other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
				_, err = other.CreateRunningInstance(context.Background(), s)
				require.ErrorContains(t, err, "ownership mismatch")
				m.AssertNotCalled(t, "TerminateInstances", mock.Anything, mock.Anything, mock.Anything)
				m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
				m.AssertExpectations(t)
			})
		}
	}
}

func TestRetryBeforeTaggingRequiresOriginalSpareName(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(fmt.Sprint(renamed), func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
			candidate.Tags = append(candidate.Tags, types.Tag{Key: aws.String("Name"), Value: aws.String("bfd-spare-original")})
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			expectInitialClaimLookup(m, candidate)
			first := m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return((*ec2.ModifyInstanceAttributeOutput)(nil), fmt.Errorf("lost response")).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			require.Error(t, err)
			if renamed {
				candidate.Tags[len(candidate.Tags)-1].Value = aws.String("another-runner")
			} else {
				m.On("ModifyInstanceAttribute", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.ModifyInstanceAttributeOutput{}, nil).Once().NotBefore(first)
				m.On("CreateTags", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.CreateTagsOutput{}, nil).Once()
				m.On("StartInstances", mock.Anything, mock.Anything, mock.Anything).Return(&ec2.StartInstancesOutput{}, nil).Once()
			}
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 1 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			other := &AwsCli{cfg: cli.cfg, client: m, cacheDir: cli.cacheDir}
			id, err := other.CreateRunningInstance(context.Background(), s)
			if renamed {
				require.ErrorContains(t, err, "ownership mismatch")
			} else {
				require.NoError(t, err)
				require.Equal(t, "i-claim", id)
			}
			m.AssertNotCalled(t, "RunInstances", mock.Anything, mock.Anything, mock.Anything)
			m.AssertExpectations(t)
		})
	}
}

func expectInitialClaimLookup(m *MockComputeClient, candidate types.Instance) {
	m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
		return len(in.InstanceIds) == 1 && in.InstanceIds[0] == aws.ToString(candidate.InstanceId)
	}), mock.Anything).Return(instanceResponse(candidate), nil).Once()
}

func TestFirstClaimRechecksLiveStateAndOwnership(t *testing.T) {
	for _, change := range []string{"pool", "name", "controller", "plan", "image", "running", "stopping", "terminated", "missing"} {
		t.Run(change, func(t *testing.T) {
			cli, m := newCapacityCli(t)
			m.ExpectedCalls[0].Maybe()
			cli.cacheDir = t.TempDir()
			s := capacitySpec(false)
			s.StoppedPool = "bfd-linux"
			candidate := poolCandidate(s.ControllerID, s.StoppedPool, s.StoppedPlan(), s.BootstrapParams.Image)
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool { return len(in.InstanceIds) == 0 }), mock.Anything).Return(instanceResponse(candidate), nil).Once()
			live := candidate
			live.Tags = append([]types.Tag{}, candidate.Tags...)
			switch change {
			case "pool":
				live.Tags = append(live.Tags, types.Tag{Key: aws.String("GARM_POOL_ID"), Value: aws.String("another-pool")})
			case "name":
				live.Tags = append(live.Tags, types.Tag{Key: aws.String("Name"), Value: aws.String("another-runner")})
			case "controller":
				live.Tags[0].Value = aws.String("other-controller")
			case "plan":
				live.Tags[2].Value = aws.String("other-plan")
			case "image":
				live.ImageId = aws.String("ami-other")
			case "running":
				live.State = &types.InstanceState{Name: types.InstanceStateNameRunning}
			case "stopping":
				live.State = &types.InstanceState{Name: types.InstanceStateNameStopping}
			case "terminated":
				live.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
			}
			response := instanceResponse(live)
			if change == "missing" {
				response = &ec2.DescribeInstancesOutput{}
			}
			m.On("DescribeInstances", mock.Anything, mock.MatchedBy(func(in *ec2.DescribeInstancesInput) bool {
				return len(in.InstanceIds) == 1 && in.InstanceIds[0] == "i-claim"
			}), mock.Anything).Run(func(mock.Arguments) {
				_, err := os.Stat(filepath.Join(cli.cacheDir, "claims", "i-claim"))
				require.NoError(t, err, "live lookup must follow reservation")
			}).Return(response, nil).Once()
			_, err := cli.CreateRunningInstance(context.Background(), s)
			require.Error(t, err)
			for _, method := range []string{"ModifyInstanceAttribute", "CreateTags", "StartInstances", "TerminateInstances", "RunInstances"} {
				m.AssertNotCalled(t, method, mock.Anything, mock.Anything, mock.Anything)
			}
			m.AssertExpectations(t)
		})
	}
}
