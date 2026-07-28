package talos

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
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

const (
	extraManifestsKey = "extraManifests"

	// templatePlaceholder stands in for a Go template action while the patch
	// is parsed as YAML. It must be a plain scalar that is legal in both key
	// and value position.
	templatePlaceholder = "x-talos-template-value"

	// maxNodeDepth bounds the YAML walk. Aliases can be self-referential, and
	// an extraction helper for a security guard must terminate on hostile
	// input rather than recurse until the stack dies.
	maxNodeDepth = 100
)

var (
	// templateActionRe matches a Go text/template action, including one that
	// spans lines. Non-greedy so adjacent actions on one line stay separate.
	templateActionRe = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

	extraManifestsHeaderRe = regexp.MustCompile(`(?m)^(\s*)` + extraManifestsKey + `\s*:`)
	urlTokenRe             = regexp.MustCompile(`https?://[^\s"',\[\]{}]+`)
	rawGitHubRefRe         = regexp.MustCompile(`^https://raw\.githubusercontent\.com/[^/]+/[^/]+/([^/]+)/`)
	fullCommitSHARe        = regexp.MustCompile(`^[0-9a-f]{40}$`)

	// semverTagRe is anchored at both ends: unanchored, any ref merely
	// starting with digits and a dot ("1.0-latest", "2.0-dev") passed as an
	// immutable version. Three segments are required for the same reason — a
	// two-segment "2.0" is indistinguishable from a release branch, so it
	// falls through to the allowlist instead of being trusted. A semver
	// prerelease/build suffix stays pinned: those name tags, not branches.
	semverTagRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

// mutableRefNames are branch-like refs that must never appear as the pinned
// segment of an extraManifests URL. Not exhaustive across every hosting
// convention, but catches the common moving-target defaults.
var mutableRefNames = map[string]bool{
	"main": true, "master": true, "develop": true, "development": true,
	"trunk": true, "head": true, "latest": true,
}

// ExtractExtraManifests finds every extraManifests list entry in a Talos
// machine-config patch source, at any nesting depth and in every document of a
// multi-document patch.
//
// Extraction is a real YAML parse. The patches are Go text/template sources
// (e.g. "{{ .DefaultDisk }}") and are not valid YAML as written, which is why
// this was originally a line scanner; template actions are neutralised into a
// plain scalar first so the parser can read them. Parsing is what makes
// comments, quoting, flow sequences and block scalars ordinary rather than
// shapes each needing its own regex — every one of those silently returned
// zero entries under the line scanner, and an entry this function does not
// return is an unpinned manifest that passes the guard unseen.
//
// If the source cannot be parsed at all, extraction falls back to a
// deliberately over-inclusive text scan rather than reporting nothing: a
// malformed patch must not be a way to launder an unpinned reference past the
// check.
func ExtractExtraManifests(patchSource string) []string {
	urls, err := parseExtraManifests(patchSource)
	if err != nil {
		return scanExtraManifestURLs(patchSource)
	}
	return urls
}

func parseExtraManifests(patchSource string) ([]string, error) {
	sanitised := templateActionRe.ReplaceAllString(patchSource, templatePlaceholder)

	var urls []string
	dec := yaml.NewDecoder(strings.NewReader(sanitised))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return urls, nil
			}
			return nil, err
		}
		collectExtraManifests(&doc, 0, &urls)
	}
}

// collectExtraManifests walks every node looking for an extraManifests key,
// regardless of where it sits: the guard is not worth narrowing to
// cluster.extraManifests, since an entry anywhere in a machine config is an
// entry Talos may act on.
func collectExtraManifests(node *yaml.Node, depth int, urls *[]string) {
	if node == nil || depth > maxNodeDepth {
		return
	}
	if node.Kind == yaml.AliasNode {
		collectExtraManifests(node.Alias, depth+1, urls)
		return
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind == yaml.ScalarNode && key.Value == extraManifestsKey {
				collectScalars(value, depth+1, urls)
			}
			collectExtraManifests(value, depth+1, urls)
		}
		return
	}
	for _, child := range node.Content {
		collectExtraManifests(child, depth+1, urls)
	}
}

// collectScalars flattens whatever sits under an extraManifests key. Talos
// expects a list of strings; anything else is malformed, and collecting its
// scalars anyway keeps a stray shape visible to the pin check instead of
// dropping it.
func collectScalars(node *yaml.Node, depth int, urls *[]string) {
	if node == nil || depth > maxNodeDepth {
		return
	}
	if node.Kind == yaml.AliasNode {
		collectScalars(node.Alias, depth+1, urls)
		return
	}
	if node.Kind == yaml.ScalarNode {
		if v := strings.TrimSpace(node.Value); v != "" {
			*urls = append(*urls, v)
		}
		return
	}
	for _, child := range node.Content {
		collectScalars(child, depth+1, urls)
	}
}

// scanExtraManifestURLs is the unparseable-source fallback. It takes every
// URL-looking token from an extraManifests key's line and the indented lines
// below it, without caring about item structure. Over-reporting (a URL sitting
// in a comment inside the block) is the safe direction: it fails the check and
// gets a human's attention, where a miss would not.
func scanExtraManifestURLs(patchSource string) []string {
	var urls []string
	headerIndent := -1
	for _, line := range strings.Split(patchSource, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := extraManifestsHeaderRe.FindStringSubmatch(line); m != nil {
			headerIndent = len(m[1])
			urls = append(urls, urlTokenRe.FindAllString(line, -1)...)
			continue
		}
		if headerIndent < 0 {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent <= headerIndent && !strings.HasPrefix(trimmed, "#") {
			headerIndent = -1
			continue
		}
		urls = append(urls, urlTokenRe.FindAllString(line, -1)...)
	}
	return urls
}

// ClassifyManifestRef reports the git ref a raw.githubusercontent.com URL is
// pinned to and whether that ref is immutable (a full 40-char commit SHA or a
// full three-segment version tag) rather than a moving branch name.
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
