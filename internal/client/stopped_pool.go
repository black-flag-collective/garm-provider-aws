package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	// Serialize all retries for this GARM runner across provider processes. The
	// record survives both process exit and an ambiguous EC2 response.
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(s.ControllerID+"/"+s.BootstrapParams.PoolID+"/"+s.BootstrapParams.Name)))
	runnerDir := filepath.Join(a.cacheDir, "runner-claims")
	if err := os.MkdirAll(runnerDir, 0700); err != nil {
		return "", err
	}
	record, err := os.OpenFile(filepath.Join(runnerDir, key+".lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return "", err
	}
	defer record.Close()
	if err := lockClaim(record); err != nil {
		return "", err
	}
	data, err := os.ReadFile(record.Name() + ".json")
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var claim runnerClaim
	if len(data) != 0 {
		if err := json.Unmarshal(data, &claim); err != nil {
			return "", fmt.Errorf("read runner claim: %w", err)
		}
	}
	if claim.Cold != nil {
		return a.createColdInstance(ctx, s, userData, record, &claim)
	}
	id, err := a.findStopped(ctx, s, cli, decoded, claimDir, record, &claim)
	if id != "" || err != nil {
		return id, err
	}
	// Keep the runner lock through fallback and persist its request/result too.
	return a.createColdInstance(ctx, s, userData, record, &claim)
}

func (a *AwsCli) findStopped(ctx context.Context, s *spec.RunnerSpec, cli stoppedClient, decoded []byte, claimDir string, record *os.File, claim *runnerClaim) (string, error) {
	if claim.ID != "" {
		return a.resumeStopped(ctx, s, cli, decoded, claimDir, record, claim)
	}
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
			// Persist the selected instance before reserving it. No EC2 mutation can
			// happen without this runner's recovery record already on disk.
			*claim = runnerClaim{ID: id, Plan: s.StoppedPlan(), SpareName: tagValue(instance.Tags, "Name"), BootstrapDigest: fmt.Sprintf("%x", sha256.Sum256(decoded))}
			if err := saveRunnerClaim(record, *claim); err != nil {
				return "", err
			}
			lease, err := reserveStopped(claimDir, id, s.BootstrapParams.Name)
			if os.IsExist(err) {
				*claim = runnerClaim{}
				if err := saveRunnerClaim(record, *claim); err != nil {
					return "", err
				}
				continue
			}
			if err != nil {
				return "", err
			}
			defer lease.Close()
			return a.startClaimed(ctx, s, cli, decoded, record, claim)

		}
	}
	return "", nil
}

// Starting is persisted before StartInstances, so a retry never rewrites the
// bootstrap of a machine whose start may have succeeded.
type runnerClaim struct {
	ID              string     `json:"id"`
	Plan            string     `json:"plan,omitempty"`
	SpareName       string     `json:"spare_name"`
	BootstrapDigest string     `json:"bootstrap_digest"`
	Starting        bool       `json:"starting"`
	Rejected        bool       `json:"rejected"`
	Cold            *coldClaim `json:"cold,omitempty"`
}

// The lock inode is permanent. State is replaced atomically beside it, so
// readers cannot lock an obsolete inode after a rename or observe torn JSON.
func saveRunnerClaim(lock *os.File, claim runnerClaim) error {
	data, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	dir := filepath.Dir(lock.Name())
	temp, err := os.CreateTemp(dir, ".claim-state-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), lock.Name()+".json"); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Publish complete ownership atomically. A crash cannot leave an empty claim
// file that another process mistakes for a different runner's reservation.
func reserveStopped(dir, id, name string) (*os.File, error) {
	path := filepath.Join(dir, id)
	if f, err := os.OpenFile(path, os.O_RDWR, 0600); err == nil {
		if err := lockClaim(f); err != nil {
			f.Close()
			return nil, err
		}
		data, err := io.ReadAll(f)
		if err != nil || strings.TrimSpace(string(data)) != name {
			f.Close()
			if err != nil {
				return nil, err
			}
			return nil, os.ErrExist
		}
		return f, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".claim-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err = lockClaim(f); err == nil {
		_, err = fmt.Fprintln(f, name)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = os.Link(f.Name(), path)
	}
	if err == nil {
		err = syncDirectory(dir)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (a *AwsCli) resumeStopped(ctx context.Context, s *spec.RunnerSpec, cli stoppedClient, decoded []byte, dir string, record *os.File, claim *runnerClaim) (string, error) {
	if claim.Plan != s.StoppedPlan() {
		return "", fmt.Errorf("persisted stopped claim does not match requested plan")
	}
	if claim.BootstrapDigest != fmt.Sprintf("%x", sha256.Sum256(decoded)) {
		return "", fmt.Errorf("stopped runner bootstrap parameters changed")
	}
	if filepath.Base(claim.ID) != claim.ID {
		return "", fmt.Errorf("invalid persisted instance ID")
	}
	lease, err := reserveStopped(dir, claim.ID, s.BootstrapParams.Name)
	if os.IsExist(err) {
		// A crash between persisting the selection and acquiring the reservation.
		// Nothing was mutated; clear it and let the next retry select another spare.
		if err := saveRunnerClaim(record, runnerClaim{}); err != nil {
			return "", err
		}
		return "", fmt.Errorf("selected spare belongs to another runner; retry selection")
	}
	if err != nil {
		return "", err
	}
	defer lease.Close()
	response, err := a.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{claim.ID}})
	if err != nil {
		return "", fmt.Errorf("describe claimed runner: %w", err)
	}
	for _, reservation := range response.Reservations {
		for _, instance := range reservation.Instances {
			if aws.ToString(instance.InstanceId) != claim.ID || instance.State == nil {
				continue
			}
			if tagValue(instance.Tags, "GARM_CONTROLLER_ID") != s.ControllerID {
				return "", fmt.Errorf("claimed runner controller mismatch")
			}
			if aws.ToString(instance.ImageId) != s.BootstrapParams.Image || tagValue(instance.Tags, "BFD_STOPPED_PLAN") != claim.Plan {
				return "", fmt.Errorf("claimed runner does not match requested plan")
			}
			owned := tagValue(instance.Tags, "GARM_POOL_ID") == s.BootstrapParams.PoolID && tagValue(instance.Tags, "Name") == s.BootstrapParams.Name
			unclaimed := !claim.Starting && !claim.Rejected && tagValue(instance.Tags, "GARM_POOL_ID") == "" && tagValue(instance.Tags, "Name") == claim.SpareName && tagValue(instance.Tags, "BFD_STOPPED_POOL") == s.StoppedPool
			if !owned && !unclaimed {
				return "", fmt.Errorf("claimed runner ownership mismatch")
			}
			switch instance.State.Name {
			case types.InstanceStateNamePending, types.InstanceStateNameRunning:
				if !owned {
					return "", fmt.Errorf("claimed runner ownership not yet visible")
				}
				return claim.ID, nil
			case types.InstanceStateNameStopped:
				if claim.Rejected {
					if err := a.TerminateInstance(ctx, claim.ID); err != nil {
						return "", err
					}
					return "", nil
				}
				// Ownership tags are published after user data. Recover a lost
				// CreateTags response without trying to retag a claimed spare.
				if !claim.Starting && owned {
					claim.Starting = true
					if err := saveRunnerClaim(record, *claim); err != nil {
						return "", err
					}
				}
				return a.startClaimed(ctx, s, cli, decoded, record, claim)
			case types.InstanceStateNameTerminated:
				if claim.Rejected {
					return "", nil
				}
			}
			return "", fmt.Errorf("claimed runner state %s is not ready", instance.State.Name)
		}
	}
	return "", fmt.Errorf("claimed runner not yet visible")
}

func (a *AwsCli) startClaimed(ctx context.Context, s *spec.RunnerSpec, cli stoppedClient, decoded []byte, record *os.File, claim *runnerClaim) (string, error) {
	id := claim.ID
	if !claim.Starting {
		_, err := cli.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: aws.String(id), UserData: &types.SecureBlobAttributeValue{Value: decoded}})
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
		if _, err := cli.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: tags}); err != nil {
			return "", fmt.Errorf("tag stopped runner: %w", err)
		}
		claim.Starting = true
		if err := saveRunnerClaim(record, *claim); err != nil {
			return "", err
		}
	}
	if _, err := a.client.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
		if !isCapacityError(err) {
			return "", fmt.Errorf("start claimed runner (outcome may be uncertain): %w", err)
		}
		// Confirm termination before allowing cold fallback. A retry recovers the
		// same reservation even if either the start or termination response is lost.
		claim.Rejected = true
		if err := saveRunnerClaim(record, *claim); err != nil {
			return "", err
		}
		if err := a.TerminateInstance(ctx, id); err != nil {
			return "", err
		}
		return "", nil
	}
	fmt.Fprintf(os.Stderr, "garm-provider-aws: started stopped pool instance %s at %s\n", id, time.Now().UTC().Format(time.RFC3339))
	return id, nil
}
