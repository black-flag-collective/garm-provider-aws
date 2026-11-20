package spec

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestEarlyBootstrapGuard(t *testing.T) {
	p := params.BootstrapInstance{Name: "runner", OSType: params.Linux, JitConfigEnabled: true, ExtraSpecs: json.RawMessage(`{"early_bootstrap":true}`)}
	p.UserDataOptions.DisableUpdatesOnBoot = true
	tools := params.RunnerApplicationDownload{OS: aws.String("linux"), Architecture: aws.String("amd64"), DownloadURL: aws.String("https://example.test/runner"), Filename: aws.String("runner.tgz")}
	data, err := linuxCloudConfig(p, tools)
	require.NoError(t, err)
	var config struct {
		Files []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(data), &config))
	require.Len(t, config.Files, 1)
	script, err := base64.StdEncoding.DecodeString(config.Files[0].Content)
	require.NoError(t, err)
	require.Contains(t, string(script), "# bfd-early-bootstrap-v1")
	require.Contains(t, string(script), "exec sudo /usr/local/sbin/bfd-bootstrap-once")
	require.Contains(t, string(script), "DefaultDependencies=no")
	p.UserDataOptions.DisableUpdatesOnBoot = false
	_, err = linuxCloudConfig(p, tools)
	require.ErrorContains(t, err, "requires a preconfigured JIT image")
	p.ExtraSpecs = json.RawMessage(`{}`)
	data, err = linuxCloudConfig(p, tools)
	require.NoError(t, err)
	require.NotContains(t, data, "bfd-early-bootstrap")
}

func TestEarlyBootstrapRejectsNonLinuxPool(t *testing.T) {
	for _, osType := range []params.OSType{params.Windows, params.Linux} {
		t.Run(string(osType), func(t *testing.T) {
			s := &RunnerSpec{Region: "us-east-1", EarlyBootstrap: true, BootstrapParams: params.BootstrapInstance{Name: "runner", OSType: osType}}
			err := s.Validate()
			if osType == params.Linux {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "early_bootstrap requires Linux")
			}
		})
	}
}
