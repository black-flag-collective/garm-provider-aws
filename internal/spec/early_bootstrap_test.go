package spec

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	assertGeneratedEarlyUnitRuns(t, script)
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

func assertGeneratedEarlyUnitRuns(t *testing.T, script []byte) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("generated Linux bootstrap execution requires bash")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "generated-bootstrap.sh")
	require.NoError(t, os.WriteFile(path, script, 0600))
	output, err := exec.Command(bash, "-n", path).CombinedOutput()
	require.NoError(t, err, string(output))
	// Execute the actual generated transformation, decoded from cloud-init's
	// write_files payload, against a service fixture. Other provisioning commands
	// are syntax-checked above; they are never run on the development machine.
	_, body, ok := strings.Cut(string(script), "# bfd-early-unit-begin\n")
	require.True(t, ok)
	function, _, ok := strings.Cut(body, "# bfd-early-unit-end")
	require.True(t, ok)
	unit := filepath.Join(dir, "runner with spaces.service")
	before := "[Unit]\nDescription=runner\nAfter=network.target\n[Service]\nExecStart=/home/runner/runsvc.sh\n"
	after := "[Unit]\nDescription=runner\nDefaultDependencies=no\nAfter=local-fs.target systemd-networkd.service\n[Service]\nExecStart=/home/runner/runsvc.sh\n"
	require.NoError(t, os.WriteFile(unit, []byte(before), 0600))
	for i := 0; i < 2; i++ {
		output, err = exec.Command(bash, "-eu", "-c", function+"\nbfdConfigureEarlyUnit \"$1\"", "test", unit).CombinedOutput()
		require.NoError(t, err, string(output))
		result, err := os.ReadFile(unit)
		require.NoError(t, err)
		require.Equal(t, after, string(result))
	}
	// A failed write must propagate instead of installing the original gated unit.
	output, err = exec.Command(bash, "-eu", "-c", function+"\nbfdConfigureEarlyUnit \"$1\"", "test", filepath.Join(dir, "missing", "runner.service")).CombinedOutput()
	require.Error(t, err, string(output))
}
