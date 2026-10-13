package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// sopsConfig is the subset of .sops.yaml that talops manages.
type sopsConfig struct {
	CreationRules []creationRule `yaml:"creation_rules"`
}

type creationRule struct {
	PathRegex string `yaml:"path_regex"`
	Age       string `yaml:"age"`
}

// SopsConfigPath locates the .sops.yaml controlling the vault by walking up from
// the current directory. Returns the conventional repo-root path when none is
// found yet (so callers can create it).
func SopsConfigPath() string {
	dir, err := os.Getwd()
	if err != nil {
		return ".sops.yaml"
	}
	for {
		candidate := filepath.Join(dir, ".sops.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ".sops.yaml"
}

// Recipients returns the age public keys configured for the vault.
func Recipients() ([]string, error) {
	path := SopsConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg sopsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range cfg.CreationRules {
		for _, k := range splitRecipients(r.Age) {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out, nil
}

// AddRecipient adds an age public key to every creation rule in .sops.yaml and
// re-encrypts each existing vault file to the new recipient set.
func (v *Vault) AddRecipient(ctx context.Context, pubKey string) error {
	if err := v.Available(); err != nil {
		return err
	}
	pubKey = strings.TrimSpace(pubKey)
	if !strings.HasPrefix(pubKey, "age1") {
		return fmt.Errorf("not a valid age public key (expected age1...): %q", pubKey)
	}

	path := SopsConfigPath()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	out, changed, err := addRecipientToConfig(data, pubKey)
	if err != nil {
		return fmt.Errorf("update %s: %w", path, err)
	}
	if len(data) == 0 {
		v.logger.Info("creating .sops.yaml", zap.String("config", path))
	}
	if !changed {
		v.logger.Info("recipient already present, nothing to do", zap.String("pubkey", pubKey))
		return nil
	}

	if err := writeFileAtomic(path, out, 0644); err != nil {
		return err
	}
	v.logger.Info("added recipient to .sops.yaml", zap.String("pubkey", pubKey), zap.String("config", path))

	for _, e := range v.Entries() {
		if _, err := os.Stat(e.Enc); err != nil {
			continue
		}
		if err := v.updateKeys(ctx, e.Enc); err != nil {
			return err
		}
		v.logger.Info("rekeyed vault file", zap.String("enc", e.Enc))
	}
	return nil
}

// RecipientPlan describes what AddRecipient would do without doing it.
type RecipientPlan struct {
	// Config is the .sops.yaml that would be edited.
	Config string
	// Present is true when AddRecipient would change nothing.
	Present bool
	// Rekey lists the existing vault files that would be re-encrypted.
	Rekey []string
}

// DecryptTFVars returns the plaintext of the encrypted tfvars without writing
// it anywhere. ok is false when there is no encrypted tfvars to read; an
// existing file that cannot be decrypted is an error, never a silent skip.
func (v *Vault) DecryptTFVars(ctx context.Context) (data []byte, ok bool, err error) {
	enc := v.TFVarsEntry().Enc
	if _, statErr := os.Stat(enc); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat %s: %w", enc, statErr)
	}
	data, err = v.decrypt(ctx, enc)
	if err != nil {
		return nil, false, fmt.Errorf("decrypt %s: %w", enc, err)
	}
	return data, true, nil
}

// PlanAddRecipient reports the effect of AddRecipient(pubKey) while leaving
// .sops.yaml and the vault untouched.
func (v *Vault) PlanAddRecipient(pubKey string) (RecipientPlan, error) {
	pubKey = strings.TrimSpace(pubKey)
	if !strings.HasPrefix(pubKey, "age1") {
		return RecipientPlan{}, fmt.Errorf("not a valid age public key (expected age1...): %q", pubKey)
	}
	plan := RecipientPlan{Config: SopsConfigPath()}
	data, err := os.ReadFile(plan.Config)
	if err != nil && !os.IsNotExist(err) {
		return RecipientPlan{}, fmt.Errorf("read %s: %w", plan.Config, err)
	}
	// Same edit AddRecipient applies, so the preview cannot disagree with it.
	_, changed, err := addRecipientToConfig(data, pubKey)
	if err != nil {
		return RecipientPlan{}, fmt.Errorf("update %s: %w", plan.Config, err)
	}
	plan.Present = !changed
	if plan.Present {
		return plan, nil
	}
	for _, e := range v.Entries() {
		if _, err := os.Stat(e.Enc); err == nil {
			plan.Rekey = append(plan.Rekey, e.Enc)
		}
	}
	return plan, nil
}

// addRecipientToConfig appends pubKey to every creation rule's age list by
// editing the YAML node tree in place: a struct round-trip would drop the
// comments (which record why each rule exists) and any rule keys talops does
// not model. A file without creation rules is bootstrapped with a single rule.
func addRecipientToConfig(data []byte, pubKey string) (out []byte, changed bool, err error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, false, fmt.Errorf("parse: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("top level is not a mapping")
	}

	rules := mappingValue(root, "creation_rules")
	if rules == nil {
		rules = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, scalarNode("creation_rules"), rules)
	}
	if rules.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("creation_rules is not a list")
	}
	if len(rules.Content) == 0 {
		rules.Content = append(rules.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			scalarNode("path_regex"), scalarNode(`\.enc\.ya?ml$`),
		}})
	}

	for _, rule := range rules.Content {
		if rule.Kind != yaml.MappingNode {
			return nil, false, fmt.Errorf("creation rule is not a mapping")
		}
		age := mappingValue(rule, "age")
		if age == nil {
			rule.Content = append(rule.Content, scalarNode("age"), scalarNode(pubKey))
			changed = true
			continue
		}
		if age.Kind != yaml.ScalarNode {
			return nil, false, fmt.Errorf("creation rule %q: age must be a comma-separated string", ruleName(rule))
		}
		var keys []string
		if age.Tag != "!!null" {
			keys = splitRecipients(age.Value)
		}
		if containsStr(keys, pubKey) {
			continue
		}
		age.Value = strings.Join(append(keys, pubKey), ",")
		// An empty `age:` parses as a null; without resetting the tag the
		// value would be written back as null.
		age.Tag, age.Style = "!!str", 0
		changed = true
	}
	if !changed {
		return data, false, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(detectIndent(data))
	if err := enc.Encode(&doc); err != nil {
		return nil, false, fmt.Errorf("marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, false, fmt.Errorf("marshal: %w", err)
	}
	return buf.Bytes(), true, nil
}

func ruleName(rule *yaml.Node) string {
	if re := mappingValue(rule, "path_regex"); re != nil && re.Value != "" {
		return re.Value
	}
	return "(unnamed)"
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// detectIndent returns the file's own indent width so re-encoding does not
// reflow it; the yaml encoder's default of 4 is used for new files.
func detectIndent(data []byte) int {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if n := len(line) - len(trimmed); n > 0 && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return n
		}
	}
	return 4
}

func splitRecipients(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
