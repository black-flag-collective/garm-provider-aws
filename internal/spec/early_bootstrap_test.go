package spec

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// This fixture is GARM v0.2.1's internal/templates/userdata/linux_wrapper.tmpl.
// Unlike provider-common's inline default, it downloads the actual template.
func TestEarlyBootstrapGarmWrapperExecutesDownloadedScript(t *testing.T) {
	wrapper, err := os.ReadFile("testdata/garm-v0.2.1-linux-wrapper.tmpl")
	require.NoError(t, err)
	extra, err := json.Marshal(map[string]any{"early_bootstrap": true, "runner_install_template": wrapper})
	require.NoError(t, err)
	p := params.BootstrapInstance{Name: "runner", OSType: params.Linux, JitConfigEnabled: true, ExtraSpecs: extra}
	p.UserDataOptions.DisableUpdatesOnBoot = true
	tools := params.RunnerApplicationDownload{OS: aws.String("linux"), Architecture: aws.String("amd64"), DownloadURL: aws.String("https://example.test/runner"), Filename: aws.String("runner.tgz")}
	data, err := linuxCloudConfig(p, tools)
	require.NoError(t, err)
	compressed, err := maybeCompressUserdata([]byte(data), params.Linux)
	require.NoError(t, err)
	require.Less(t, len(compressed), 16*1024)
	var config struct {
		Files []struct {
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(data), &config))
	script, err := base64.StdEncoding.DecodeString(config.Files[0].Content)
	require.NoError(t, err)
	dir := t.TempDir()
	helper := filepath.Join(dir, "bfd-bootstrap-once")
	require.NoError(t, os.WriteFile(helper, []byte("#!/bin/bash\nexit 99\n"), 0700))
	downloaded := filepath.Join(dir, "real-install.sh")
	body := strings.ReplaceAll(string(script), "/usr/local/sbin/bfd-bootstrap-once", helper)
	body = strings.ReplaceAll(body, "/tmp/real-install.sh", downloaded)
	path := filepath.Join(dir, "bootstrap.sh")
	require.NoError(t, os.WriteFile(path, []byte(body), 0700))
	out, err := exec.Command("bash", "-n", path).CombinedOutput()
	require.NoError(t, err, string(out))
	// Mock network and privileged install boundaries, but execute the complete
	// generated wrapper and verify rejection before any downloaded command runs.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/bash\ncp \"$BFD_FIXTURE\" \"${@: -1}\"\n"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/bash\n[[ $1 == mv ]] || exit 98\ncp \"$2\" \"$BFD_RESULT\"\n"), 0700))
	fixture := filepath.Join(dir, "install-fixture.sh")
	result := filepath.Join(dir, "installed.service")
	templates, err := supportedGarmScripts()
	require.NoError(t, err)
	for i, expected := range templates {
		t.Run(fmt.Sprintf("complete upstream template debug %d", i), func(t *testing.T) {
			// Validate and transform the real rendered template without executing its
			// privileged provisioning. The unit helper is execution-tested separately.
			content := regexp.MustCompile(templateScalar+`[A-Za-z]+_`).ReplaceAllString(expected, "runner")
			require.NoError(t, os.WriteFile(fixture, []byte(content), 0700))
			start := strings.Index(body, "# bfd-early-unit-begin")
			end := strings.Index(body, "# bfd-early-prepare-end")
			require.Greater(t, end, start)
			prepare := body[start:end] + "\nbfdPrepareEarlyInstall \"$1\""
			out, err := exec.Command("bash", "-c", prepare, "test", fixture).CombinedOutput()
			require.NoError(t, err, string(out))
			transformed, err := os.ReadFile(fixture)
			require.NoError(t, err)
			require.Contains(t, string(transformed), "bfdConfigureEarlyUnit \"$SVC_NAME\" || exit 1\n")
			require.NoError(t, exec.Command("bash", "-n", fixture).Run())
		})
	}
	const install = `sudo mv $SVC_NAME /etc/systemd/system/ || fail "failed to move service file"`
	for _, tc := range []struct {
		name string
		line string
	}{
		{"standalone command is not a supported template", install},
		{"indented command is not a supported template", "\t " + install + " \t"},
		{"unreachable branch", "if false; then\n" + install + "\nfi\nsudo mv runner.service /etc/systemd/system/"},
		{"uncalled function", "unused() {\n" + install + "\n}\nsudo mv runner.service /etc/systemd/system/"},
		{"heredoc", "cat <<'EOF'\n" + install + "\nEOF"},
		{"changed whole template", "if false; then\n" + regexp.MustCompile(templateScalar+`[A-Za-z]+_`).ReplaceAllString(templates[0], "runner") + "\nfi"},
		{"injected scalar", regexp.MustCompile(templateScalar+`[A-Za-z]+_`).ReplaceAllLiteralString(templates[0], "$(touch invalid)")},
		{"missing command", "true"},
		{"comment", "# " + install},
		{"echoed string", "echo '" + install + "'"},
		{"compound line", install + "; echo unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(fixture, []byte(tc.line), 0700))
			start := strings.Index(body, "# bfd-early-unit-begin")
			end := strings.Index(body, "# bfd-early-prepare-end")
			prepare := body[start:end] + "\nbfdPrepareEarlyInstall \"$1\""
			output, prepareErr := exec.Command("bash", "-c", prepare, "test", fixture).CombinedOutput()
			require.Error(t, prepareErr, string(output))
			unchanged, readErr := os.ReadFile(fixture)
			require.NoError(t, readErr)
			require.Equal(t, tc.line, string(unchanged))
			// A preceding side effect must not run even when an exact install
			// command exists in a comment, dead branch or unused function.
			content := "#!/bin/bash\necho should-not-run > \"$BFD_RESULT\"\n" + tc.line + "\n"
			require.NoError(t, os.WriteFile(fixture, []byte(content), 0700))
			_ = os.Remove(result)
			cmd := exec.Command("bash", path)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "BFD_BOOTSTRAP_LOCKED=1", "BFD_FIXTURE="+fixture, "BFD_RESULT="+result)
			out, err := cmd.CombinedOutput()
			require.Error(t, err, string(out))
			require.Contains(t, string(out), "unsupported downloaded install template")
			require.NoFileExists(t, result)
		})
	}
}
