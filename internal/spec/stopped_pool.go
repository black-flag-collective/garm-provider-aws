package spec

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// StoppedPlan is shared with runner-os's keeper. JSON is a compact string
// array, with sorted security groups and configured placement order retained.
// Credentials and bootstrap data are deliberately excluded.
func (s *RunnerSpec) StoppedPlan() string {
	groups := append([]string{}, s.SecurityGroupIds...)
	sort.Strings(groups)
	profile := ""
	if s.IAMInstanceProfile != nil {
		profile = aws.ToString(s.IAMInstanceProfile.Arn)
		if profile == "" && aws.ToString(s.IAMInstanceProfile.Name) != "" {
			profile = "name:" + aws.ToString(s.IAMInstanceProfile.Name)
		}
	}
	values := []string{s.BootstrapParams.Image, strings.Join(append([]string{s.BootstrapParams.Flavor}, s.FallbackFlavors...), ","), strings.Join(append([]string{s.SubnetID}, s.FallbackSubnets...), ","), strings.Join(groups, ","), profile, fmt.Sprint(aws.ToInt32(s.VolumeSize)), string(s.VolumeType), fmt.Sprint(aws.ToInt32(s.Iops)), fmt.Sprint(aws.ToInt32(s.Throughput)), aws.ToString(s.SSHKeyName)}
	data, _ := json.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
