package talos

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed patches/manifest-pin-allowlist.yaml
var manifestPinAllowlistYAML string

// ManifestRef describes one cluster.extraManifests entry and whether its git
// ref is pinned to something immutable.
type ManifestRef struct {
	URL    string
	Ref    string // path segment after owner/repo, e.g. "main", "v1.2.3", or a commit SHA
	Pinned bool
}

var (
	extraManifestsHeaderRe = regexp.MustCompile(`(?m)^(\s*)extraManifests:\s*$`)
	extraManifestsItemRe   = regexp.MustCompile(`^(\s*)-\s*(\S+)\s*$`)
	rawGitHubRefRe         = regexp.MustCompile(`^https://raw\.githubusercontent\.com/[^/]+/[^/]+/([^/]+)/`)
	fullCommitSHARe        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	semverTagRe            = regexp.MustCompile(`^v?[0-9]+\.[0-9]+(\.[0-9]+)?`)
)

// mutableRefNames are branch-like refs that must never appear as the pinned
// segment of an extraManifests URL. Not exhaustive across every hosting
// convention, but catches the common moving-target defaults.
var mutableRefNames = map[string]bool{
	"main": true, "master": true, "develop": true, "development": true,
	"trunk": true, "head": true, "latest": true,
}

// ExtractExtraManifests finds every cluster.extraManifests list entry in a
// Talos machine-config patch source.
//
// This scans line-by-line with indentation tracking rather than doing a full
// YAML parse, because these patch files are Go text/template sources (e.g.
// "{{ .DefaultDisk }}") before rendering and are not valid YAML as-is — the
// same reason platform's tools/sync-monitoring-crds.py uses regex matching
// instead of a YAML parse for its CRD version comparison.
func ExtractExtraManifests(patchSource string) []string {
	lines := strings.Split(patchSource, "\n")
	var urls []string
	inBlock := false
	headerIndent := -1
	itemIndent := -1
	for _, line := range lines {
		if m := extraManifestsHeaderRe.FindStringSubmatch(line); m != nil {
			inBlock = true
			headerIndent = len(m[1])
			itemIndent = -1
			continue
		}
		if !inBlock {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= headerIndent {
			inBlock = false
			continue
		}
		if m := extraManifestsItemRe.FindStringSubmatch(line); m != nil {
			if itemIndent == -1 {
				itemIndent = len(m[1])
			}
			if len(m[1]) == itemIndent {
				urls = append(urls, m[2])
			}
		}
	}
	return urls
}

// ClassifyManifestRef reports the git ref a raw.githubusercontent.com URL is
// pinned to and whether that ref is immutable (a full 40-char commit SHA or
// a version tag) rather than a moving branch name.
//
// URLs this function does not recognise the layout of (i.e. not
// raw.githubusercontent.com/<owner>/<repo>/<ref>/...) are conservatively
// treated as unpinned so they surface for human review instead of silently
// passing.
func ClassifyManifestRef(url string) (ref string, pinned bool) {
	m := rawGitHubRefRe.FindStringSubmatch(url)
	if m == nil {
		return "", false
	}
	ref = m[1]
	if mutableRefNames[strings.ToLower(ref)] {
		return ref, false
	}
	if fullCommitSHARe.MatchString(ref) || semverTagRe.MatchString(ref) {
		return ref, true
	}
	return ref, false
}

// manifestPinException is one documented allowlist entry: a URL substring
// exempt from the pinning requirement, with a mandatory human-readable
// reason.
type manifestPinException struct {
	URL    string `yaml:"url"`
	Reason string `yaml:"reason"`
}

type manifestPinAllowlist struct {
	Exceptions []manifestPinException `yaml:"exceptions"`
}

// LoadManifestPinAllowlist parses the embedded allowlist and validates that
// every entry carries a non-empty reason.
func LoadManifestPinAllowlist() (map[string]string, error) {
	var parsed manifestPinAllowlist
	if err := yaml.Unmarshal([]byte(manifestPinAllowlistYAML), &parsed); err != nil {
		return nil, fmt.Errorf("parsing patches/manifest-pin-allowlist.yaml: %w", err)
	}
	byURL := make(map[string]string, len(parsed.Exceptions))
	for i, e := range parsed.Exceptions {
		if e.URL == "" {
			return nil, fmt.Errorf("patches/manifest-pin-allowlist.yaml: exceptions[%d] missing required 'url'", i)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return nil, fmt.Errorf(
				"patches/manifest-pin-allowlist.yaml: exceptions[%d] (url=%s) missing a non-empty 'reason' — "+
					"every exception must document inline why it is deliberate", i, e.URL,
			)
		}
		if _, dup := byURL[e.URL]; dup {
			return nil, fmt.Errorf("patches/manifest-pin-allowlist.yaml: duplicate exception entry for url=%s", e.URL)
		}
		byURL[e.URL] = e.Reason
	}
	return byURL, nil
}

// CheckExtraManifestPins extracts every cluster.extraManifests URL from
// patchSource and reports one ManifestRef per entry, alongside violations:
// URLs that are neither pinned nor covered by an allowlist entry.
func CheckExtraManifestPins(patchSource string, allowlist map[string]string) (refs []ManifestRef, violations []string) {
	for _, url := range ExtractExtraManifests(patchSource) {
		ref, pinned := ClassifyManifestRef(url)
		refs = append(refs, ManifestRef{URL: url, Ref: ref, Pinned: pinned})
		if pinned {
			continue
		}
		if _, allowed := allowlist[url]; allowed {
			continue
		}
		violations = append(violations, url)
	}
	return refs, violations
}

// CheckAllExtraManifestPins runs CheckExtraManifestPins over every named
// patch source, loading the embedded allowlist once, and additionally
// reports stale allowlist entries — ones that matched no unpinned URL in any
// source — so the allowlist can't quietly rot once a reference is fixed or
// removed.
func CheckAllExtraManifestPins(patchSources map[string]string) (refs map[string][]ManifestRef, violations map[string][]string, stale []string, err error) {
	allowlist, err := LoadManifestPinAllowlist()
	if err != nil {
		return nil, nil, nil, err
	}

	refs = make(map[string][]ManifestRef, len(patchSources))
	violations = make(map[string][]string, len(patchSources))
	consumed := make(map[string]bool, len(allowlist))

	for name, source := range patchSources {
		r, v := CheckExtraManifestPins(source, allowlist)
		refs[name] = r
		violations[name] = v
		for _, ref := range r {
			if !ref.Pinned {
				if _, ok := allowlist[ref.URL]; ok {
					consumed[ref.URL] = true
				}
			}
		}
	}

	for url := range allowlist {
		if !consumed[url] {
			stale = append(stale, url)
		}
	}

	return refs, violations, stale, nil
}
