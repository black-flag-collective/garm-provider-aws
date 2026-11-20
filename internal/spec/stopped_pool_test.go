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
