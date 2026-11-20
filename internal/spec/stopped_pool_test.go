package spec

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStoppedPlanMatchesKeeper(t *testing.T) {
	s := &RunnerSpec{BootstrapParams: params.BootstrapInstance{Image: "ami-test", Flavor: "m8a.large"}, FallbackFlavors: []string{"m7a.large"}, SubnetID: "subnet-a", FallbackSubnets: []string{"subnet-b"}, SecurityGroupIds: []string{"sg-b", "sg-a"}, VolumeSize: aws.Int32(60), VolumeType: types.VolumeTypeGp3, Iops: aws.Int32(3000), Throughput: aws.Int32(125)}
	require.Equal(t, "fe02cf632c109a086512d6524e3a14fa0793a77d5f14ba4d3656b70ee0c744c0", s.StoppedPlan())
	s.BootstrapParams.Image = "ami-next"
	require.NotEqual(t, "fe02cf632c109a086512d6524e3a14fa0793a77d5f14ba4d3656b70ee0c744c0", s.StoppedPlan())
}

func TestStoppedPlanDistinguishesProfileNames(t *testing.T) {
	s := &RunnerSpec{BootstrapParams: params.BootstrapInstance{Image: "ami-test", Flavor: "m8a.large"}, FallbackFlavors: []string{"m7a.large"}, SubnetID: "subnet-a", FallbackSubnets: []string{"subnet-b"}, SecurityGroupIds: []string{"sg-b", "sg-a"}, VolumeSize: aws.Int32(60), VolumeType: types.VolumeTypeGp3, Iops: aws.Int32(3000), Throughput: aws.Int32(125)}
	empty := s.StoppedPlan()
	s.MergeExtraSpecs(&extraSpecs{IAMInstanceProfile: aws.String("runner-profile")})
	require.Equal(t, "runner-profile", aws.ToString(s.IAMInstanceProfile.Name))
	require.Nil(t, s.IAMInstanceProfile.Arn)
	named := s.StoppedPlan()
	require.NotEqual(t, empty, named)
	require.Equal(t, "50506140f37eb11a992bc8a3a75976afea88958b0222c4ea7668b6bcf87dff9c", named)
	s.MergeExtraSpecs(&extraSpecs{IAMInstanceProfile: aws.String("other-profile")})
	require.NotEqual(t, named, s.StoppedPlan())
	s.MergeExtraSpecs(&extraSpecs{IAMInstanceProfile: aws.String("arn:aws:iam::123456789012:instance-profile/runner-profile")})
	require.Nil(t, s.IAMInstanceProfile.Name)
	require.NotEmpty(t, aws.ToString(s.IAMInstanceProfile.Arn))
	require.NotEqual(t, named, s.StoppedPlan())
}
