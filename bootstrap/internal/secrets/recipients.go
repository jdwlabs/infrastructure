package secrets

import (
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
	var cfg sopsConfig
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Bootstrap the config for the first device.
	if len(cfg.CreationRules) == 0 {
		cfg.CreationRules = []creationRule{{PathRegex: `\.enc\.ya?ml$`}}
		v.logger.Info("creating .sops.yaml", zap.String("config", path))
	}

	changed := false
	for i := range cfg.CreationRules {
		keys := splitRecipients(cfg.CreationRules[i].Age)
		if containsStr(keys, pubKey) {
			continue
		}
		keys = append(keys, pubKey)
		cfg.CreationRules[i].Age = strings.Join(keys, ",")
		changed = true
	}
	if !changed {
		v.logger.Info("recipient already present, nothing to do", zap.String("pubkey", pubKey))
		return nil
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
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
	// Present is true when every creation rule already lists the key, so
	// AddRecipient would be a no-op.
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
	plan := RecipientPlan{Config: SopsConfigPath(), Present: true}
	var cfg sopsConfig
	if data, err := os.ReadFile(plan.Config); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return RecipientPlan{}, fmt.Errorf("parse %s: %w", plan.Config, err)
		}
	} else if !os.IsNotExist(err) {
		return RecipientPlan{}, fmt.Errorf("read %s: %w", plan.Config, err)
	}
	if len(cfg.CreationRules) == 0 {
		plan.Present = false
	}
	for _, r := range cfg.CreationRules {
		if !containsStr(splitRecipients(r.Age), pubKey) {
			plan.Present = false
		}
	}
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
