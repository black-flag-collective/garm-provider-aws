package spec

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	"github.com/cloudbase/garm-provider-common/params"
)

// The full upstream template is pinned so matching an install line in dead
// shell code cannot authorize an otherwise unknown program. Dynamic scalar
// fields may vary, but cannot contain shell syntax or newlines.
//
//go:embed testdata/garm-v0.2.1-github-linux.tmpl
var supportedGarmTemplate string

const templateScalar = "BFD_TEMPLATE_SCALAR_1a0ac5"

func supportedGarmScripts() ([]string, error) {
	t, err := template.New("garm").Parse(supportedGarmTemplate)
	if err != nil {
		return nil, err
	}
	var scripts []string
	for _, debug := range []bool{false, true} {
		ctx := map[string]any{"UseJITConfig": true, "AgentMode": false, "EnableBootDebug": debug}
		for _, key := range []string{"DownloadURL", "CallbackURL", "MetadataURL", "CallbackToken", "RunnerUsername", "RunnerGroup", "AgentDownloadURL", "AgentURL", "AgentToken", "AgentShell", "TempDownloadToken", "FileName"} {
			ctx[key] = templateScalar + key + "_"
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, ctx); err != nil {
			return nil, err
		}
		scripts = append(scripts, buf.String())
	}
	return scripts, nil
}

func linuxCloudConfig(p params.BootstrapInstance, tools params.RunnerApplicationDownload) (string, error) {
	var extra extraSpecs
	if len(p.ExtraSpecs) > 0 {
		if err := json.Unmarshal(p.ExtraSpecs, &extra); err != nil {
			return "", err
		}
	}
	if !extra.EarlyBootstrap {
		return cloudconfig.GetCloudConfig(p, tools, p.Name)
	}
	if !p.UserDataOptions.DisableUpdatesOnBoot || len(p.UserDataOptions.ExtraPackages) > 0 || len(extra.PreInstallScripts) > 0 || len(p.CACertBundle) > 0 || !p.JitConfigEnabled {
		return "", fmt.Errorf("early bootstrap requires a preconfigured JIT image without updates, packages, preinstall scripts or custom CA")
	}
	script, err := cloudconfig.GetRunnerInstallScript(p, tools, p.Name)
	if err != nil {
		return "", err
	}
	// The baked helper serializes early and cloud-init invocation. If absent,
	// cloud-init keeps its normal behavior, allowing a rolling image upgrade.
	guard := `#!/bin/bash
# bfd-early-bootstrap-v1
if [[ -x /usr/local/sbin/bfd-bootstrap-once && ${BFD_BOOTSTRAP_LOCKED:-} != 1 ]]; then
 exec sudo /usr/local/sbin/bfd-bootstrap-once "$0"
fi
`
	script = []byte(guard + strings.TrimPrefix(string(script), "#!/bin/bash"))
	// GARM's generated service otherwise waits for sysinit via its default
	// dependencies and network.target, undoing the early bootstrap benefit.
	old := "sudo mv $SVC_NAME /etc/systemd/system/"
	unitFunction := `# bfd-early-unit-begin
bfdConfigureEarlyUnit() {
 local unit_file="$1" line
 {
  while IFS= read -r line || [[ -n "$line" ]]; do
   if [[ "$line" == 'After=network.target' ]]; then
    printf '%s\n' 'DefaultDependencies=no' 'After=local-fs.target systemd-networkd.service'
   else
    printf '%s\n' "$line"
   fi
  done < "$unit_file"
 } > "$unit_file.bfd-early" || return 1
 mv -- "$unit_file.bfd-early" "$unit_file"
}
# bfd-early-unit-end
`
	replacement := unitFunction + `
if [[ -x /usr/local/sbin/bfd-bootstrap-once ]]; then
 bfdConfigureEarlyUnit "$SVC_NAME" || fail "failed to configure early runner service"
fi
` + old
	if strings.Contains(string(script), old) {
		script = []byte(strings.Replace(string(script), old, replacement, 1))
	} else if strings.Contains(string(script), "\n/tmp/real-install.sh\n") {
		// GARM v0.2.1 supplies a wrapper, not its actual install template. Patch
		// the downloaded script before execution, preserving GARM's callbacks.
		templates, err := supportedGarmScripts()
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(templates)
		if err != nil {
			return "", err
		}
		validator := `bfdValidateEarlyInstall() {
 python3 - "$1" '` + base64.StdEncoding.EncodeToString(encoded) + `' <<'BFD_VALIDATE_TEMPLATE'
import base64, json, pathlib, re, sys
try:
    script = pathlib.Path(sys.argv[1]).read_text()
    templates = json.loads(base64.b64decode(sys.argv[2]))
    scalar = r"[A-Za-z0-9_.-]*"
    url = r"[A-Za-z0-9_./:@?&=%+,~-]*"
    for template in templates:
        parts = re.split(r"(BFD_TEMPLATE_SCALAR_1a0ac5[A-Za-z]+_)", template)
        pattern = "".join((url if part.endswith("URL_") else scalar) if i % 2 else re.escape(part) for i, part in enumerate(parts))
        if re.fullmatch(pattern, script):
            sys.exit(0)
except (OSError, UnicodeError, ValueError):
    pass
print("early bootstrap: unsupported downloaded install template", file=sys.stderr)
sys.exit(1)
BFD_VALIDATE_TEMPLATE
}
`
		prepare := unitFunction + validator + `# bfd-early-prepare-begin
bfdWriteEarlyInstall() {
 local script="$1" line trimmed count=0
  printf '%s\n' '#!/bin/bash' || return 1
  declare -f bfdConfigureEarlyUnit || return 1
  while IFS= read -r line || [[ -n "$line" ]]; do
   trimmed="${line#"${line%%[![:space:]]*}"}"
   trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
   if [[ "$trimmed" == 'sudo mv $SVC_NAME /etc/systemd/system/ || fail "failed to move service file"' ]]; then
    printf '%s\n' 'bfdConfigureEarlyUnit "$SVC_NAME" || exit 1' || return 1
    count=$((count + 1))
   fi
   printf '%s\n' "$line" || return 1
  done < "$script"
 if [[ "$count" != 1 ]]; then
  echo 'early bootstrap: unsupported downloaded install template' >&2
  return 1
 fi
}
bfdPrepareEarlyInstall() {
 local script="$1" temporary
 bfdValidateEarlyInstall "$script" || return 1
 temporary=$(mktemp "$script.bfd-early.XXXXXX") || return 1
 bfdWriteEarlyInstall "$script" > "$temporary" && chmod 0700 "$temporary" && mv -- "$temporary" "$script" || {
  rm -f -- "$temporary"
  return 1
 }
}
# bfd-early-prepare-end
if [[ -x /usr/local/sbin/bfd-bootstrap-once ]]; then
 bfdPrepareEarlyInstall /tmp/real-install.sh || exit 1
fi
/tmp/real-install.sh
`
		script = []byte(strings.Replace(string(script), "\n/tmp/real-install.sh\n", "\n"+prepare, 1))
	} else {
		return "", fmt.Errorf("early bootstrap: unsupported runner install template")
	}
	return cloudconfig.GetCloudInitConfig(p, script)
}
