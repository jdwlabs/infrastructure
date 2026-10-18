package talos

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// talosctlVersionRe matches the first semantic version in `talosctl version
// --client` output, whether it is the short "Talos v1.14.4" form or the
// "Tag: v1.14.4" line of the full form. A pre-release suffix is ignored.
var talosctlVersionRe = regexp.MustCompile(`v(\d+)\.(\d+)\.(\d+)`)

func parseTalosctlClientVersion(output string) (string, error) {
	m := talosctlVersionRe.FindString(output)
	if m == "" {
		return "", fmt.Errorf("no version found in talosctl output %q", strings.TrimSpace(output))
	}
	return m, nil
}

func versionParts(v string) ([3]int, error) {
	var parts [3]int
	m := talosctlVersionRe.FindStringSubmatch("v" + strings.TrimPrefix(v, "v"))
	if m == nil {
		return parts, fmt.Errorf("invalid version %q", v)
	}
	for i := range parts {
		parts[i], _ = strconv.Atoi(m[i+1])
	}
	return parts, nil
}

// checkTalosctlVersion rejects a talos_version newer than the talosctl that
// would generate its configs. talosctl does not refuse a newer --talos-version;
// it quietly emits the schema it knows, so the mismatch only surfaces later as
// a config the target Talos rejects or mis-reads.
func checkTalosctlVersion(talosVersion, talosctlVersion string) error {
	want, err := versionParts(talosVersion)
	if err != nil {
		return fmt.Errorf("talos_version: %w", err)
	}
	have, err := versionParts(talosctlVersion)
	if err != nil {
		return fmt.Errorf("talosctl version: %w", err)
	}
	for i := range want {
		if want[i] < have[i] {
			return nil
		}
		if want[i] > have[i] {
			return fmt.Errorf("talos_version v%s is newer than the installed talosctl %s: "+
				"it would silently generate the older config schema; install talosctl v%s or newer",
				strings.TrimPrefix(talosVersion, "v"), talosctlVersion, strings.TrimPrefix(talosVersion, "v"))
		}
	}
	return nil
}

// ensureTalosctlSupportsPin fails before any config is generated when the
// talosctl on PATH is older than cfg.TalosVersion.
func (nc *NodeConfig) ensureTalosctlSupportsPin() error {
	out, err := nc.execCommandAudited("talosctl", "version", "--client", "--short")
	if err != nil {
		return fmt.Errorf("talosctl version: %w, output: %s", err, string(out))
	}
	have, err := parseTalosctlClientVersion(string(out))
	if err != nil {
		return err
	}
	return checkTalosctlVersion(nc.cfg.TalosVersion, have)
}
