package spec

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	"github.com/cloudbase/garm-provider-common/params"
)

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
	if !p.UserDataOptions.DisableUpdatesOnBoot || len(p.UserDataOptions.ExtraPackages) > 0 || len(extra.PreInstallScripts) > 0 || p.CACertBundle != nil || !p.JitConfigEnabled {
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
	replacement := `if [[ -x /usr/local/sbin/bfd-bootstrap-once ]]; then
 sed -i 's/^After=network.target$/DefaultDependencies=no\nAfter=local-fs.target systemd-networkd.service/' "$SVC_NAME"
fi
` + old
	if !strings.Contains(string(script), old) {
		return "", fmt.Errorf("early bootstrap: unsupported runner install template")
	}
	script = []byte(strings.Replace(string(script), old, replacement, 1))
	return cloudconfig.GetCloudInitConfig(p, script)
}
