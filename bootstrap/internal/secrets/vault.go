// Package secrets manages the encrypted SOPS vault that lets the talops
// cluster artifacts (Talos secrets bundle, talosconfig, bootstrap state, and
// terraform.tfvars) be stored securely in git and shared across machines.
//
// The committed source of truth is a set of SOPS+age encrypted files
// (`*.enc.yaml`). Plaintext working copies live in the normal, gitignored
// locations and are treated as a regenerable local cache: they are decrypted
// from the vault on demand (Hydrate) and re-encrypted back when they change
// (Seal). This keeps the day-to-day debugging experience identical to operating
// on plain files while ensuring the durable, shared copy is always encrypted.
package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"go.uber.org/zap"
)

// EnvNoAutoSeal disables the automatic Seal that runs after a successful
// command. Set it (to any non-empty value) when debugging so the tool never
// rewrites the encrypted vault behind your back.
const EnvNoAutoSeal = "TALOPS_NO_AUTOSEAL"

// Entry maps a plaintext working file to its committed encrypted vault file.
type Entry struct {
	// Plain is the plaintext working path (gitignored, regenerable cache).
	Plain string
	// Enc is the encrypted vault path committed to git.
	Enc string
	// Mode is the permission applied to a freshly hydrated plaintext file.
	Mode os.FileMode
}

// Vault encrypts and decrypts the talops secret artifacts via the `sops` binary.
type Vault struct {
	cfg    *types.Config
	logger *zap.Logger
	sops   string // resolved path to the sops binary, "" when unavailable
}

// New builds a Vault for the given config. It does not touch the filesystem.
func New(cfg *types.Config, logger *zap.Logger) *Vault {
	v := &Vault{cfg: cfg, logger: logger}
	if p, err := exec.LookPath("sops"); err == nil {
		v.sops = p
	}
	return v
}

// Available reports whether the sops binary is installed, returning an
// actionable error otherwise.
func (v *Vault) Available() error {
	if v.sops == "" {
		return fmt.Errorf("sops not found in PATH: install it (https://github.com/getsops/sops) to use the encrypted vault")
	}
	return nil
}

// KeyAvailable reports whether an age private key is reachable for decryption.
func (v *Vault) KeyAvailable() error {
	for _, p := range ageKeyCandidates() {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return nil
		}
	}
	if os.Getenv("SOPS_AGE_KEY") != "" {
		return nil
	}
	return fmt.Errorf("no age key found: generate one with `age-keygen -o ~/.config/sops/age/keys.txt` "+
		"and have an existing operator add its public key via `talops secrets add-device <pubkey>` (searched: %v)",
		ageKeyCandidates())
}

// ageKeyCandidates returns the locations sops consults for an age identity.
func ageKeyCandidates() []string {
	var out []string
	if v := os.Getenv("SOPS_AGE_KEY_FILE"); v != "" {
		out = append(out, v)
	}
	cfgHome := os.Getenv("XDG_CONFIG_HOME")
	if cfgHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfgHome = filepath.Join(home, ".config")
		}
	}
	if cfgHome != "" {
		out = append(out, filepath.Join(cfgHome, "sops", "age", "keys.txt"))
	}
	return out
}

// ClusterDir returns the per-cluster directory root.
func (v *Vault) ClusterDir() string {
	return filepath.Join("clusters", v.cfg.ClusterName)
}

// vaultDir returns the directory holding the committed encrypted artifacts.
func (v *Vault) vaultDir() string {
	return filepath.Join(v.ClusterDir(), "vault")
}

// TFVarsPlain returns the plaintext tfvars path, normalizing the bare default
// to the conventional terraform/ subdirectory so a fresh clone hydrates there.
func (v *Vault) TFVarsPlain() string {
	p := v.cfg.TerraformTFVars
	if p == "" || p == "terraform.tfvars" {
		return filepath.Join("terraform", "terraform.tfvars")
	}
	return p
}

// TFVarsEntry returns the vault entry for terraform.tfvars. The encrypted file
// lives next to the plaintext as `<name>.enc.yaml`.
func (v *Vault) TFVarsEntry() Entry {
	plain := v.TFVarsPlain()
	return Entry{Plain: plain, Enc: plain + ".enc.yaml", Mode: 0600}
}

// ClusterEntries returns the vault entries for the per-cluster artifacts.
func (v *Vault) ClusterEntries() []Entry {
	vd := v.vaultDir()
	return []Entry{
		{Plain: filepath.Join(v.cfg.SecretsDir, "secrets.yaml"), Enc: filepath.Join(vd, "secrets.enc.yaml"), Mode: 0600},
		{Plain: filepath.Join(v.cfg.SecretsDir, "talosconfig"), Enc: filepath.Join(vd, "talosconfig.enc.yaml"), Mode: 0600},
		{Plain: filepath.Join(v.ClusterDir(), "state", "bootstrap-state.json"), Enc: filepath.Join(vd, "bootstrap-state.enc.yaml"), Mode: 0600},
	}
}

// Entries returns every vault entry (tfvars + per-cluster artifacts).
func (v *Vault) Entries() []Entry {
	return append([]Entry{v.TFVarsEntry()}, v.ClusterEntries()...)
}

// EntryByName resolves a friendly artifact name to its vault entry.
// Recognized names: tfvars, secrets, talosconfig, state.
func (v *Vault) EntryByName(name string) (Entry, bool) {
	ce := v.ClusterEntries()
	switch name {
	case "tfvars", "terraform":
		return v.TFVarsEntry(), true
	case "secrets", "bundle":
		return ce[0], true
	case "talosconfig":
		return ce[1], true
	case "state", "bootstrap-state":
		return ce[2], true
	}
	return Entry{}, false
}

// EncPaths returns the encrypted vault file paths for every entry.
func (v *Vault) EncPaths() []string {
	entries := v.Entries()
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Enc
	}
	return paths
}

// HasEncrypted reports whether any encrypted vault file currently exists, i.e.
// whether there is anything to decrypt for this cluster.
func (v *Vault) HasEncrypted() bool {
	for _, e := range v.Entries() {
		if _, err := os.Stat(e.Enc); err == nil {
			return true
		}
	}
	return false
}

// Hydrate decrypts every vault file (tfvars + per-cluster) into its plaintext
// working location. See hydrateEntries for the per-file semantics.
func (v *Vault) Hydrate(ctx context.Context) ([]string, error) {
	return v.hydrateEntries(ctx, v.Entries())
}

// HydrateTFVars decrypts only terraform.tfvars. It is run first so the cluster
// name can be resolved from tfvars before the per-cluster vault paths are
// computed (the cluster artifacts live under clusters/<name>/vault).
func (v *Vault) HydrateTFVars(ctx context.Context) ([]string, error) {
	return v.hydrateEntries(ctx, []Entry{v.TFVarsEntry()})
}

// HydrateCluster decrypts the per-cluster artifacts (secrets bundle,
// talosconfig, bootstrap state). Call after the cluster name is final.
func (v *Vault) HydrateCluster(ctx context.Context) ([]string, error) {
	return v.hydrateEntries(ctx, v.ClusterEntries())
}

// hydrateEntries decrypts the given vault files into their plaintext working
// locations when the plaintext is missing or older than the encrypted source.
// It never clobbers plaintext that is newer than the vault (unsealed local
// edits) — it warns instead so the operator knows changes are pending a seal.
func (v *Vault) hydrateEntries(ctx context.Context, entries []Entry) ([]string, error) {
	if err := v.Available(); err != nil {
		return nil, err
	}
	var written []string
	for _, e := range entries {
		encInfo, err := os.Stat(e.Enc)
		if err != nil {
			continue // no vault file yet — nothing to hydrate
		}
		if plainInfo, err := os.Stat(e.Plain); err == nil {
			if plainInfo.ModTime().After(encInfo.ModTime()) {
				v.logger.Warn("plaintext is newer than the vault — keeping local edits (run `talops secrets seal` to encrypt them)",
					zap.String("plain", e.Plain))
				continue
			}
		}
		data, err := v.decrypt(ctx, e.Enc)
		if err != nil {
			return written, fmt.Errorf("decrypt %s: %w", e.Enc, err)
		}
		if err := writeFileAtomic(e.Plain, data, e.Mode); err != nil {
			return written, err
		}
		v.logger.Info("hydrated secret from vault", zap.String("plain", e.Plain), zap.String("enc", e.Enc))
		written = append(written, e.Plain)
	}
	return written, nil
}

// Seal encrypts plaintext working files into the vault when their content
// differs from what is already encrypted, avoiding spurious git churn (SOPS
// re-encryption is non-deterministic). Returns the list of vault paths it wrote.
func (v *Vault) Seal(ctx context.Context) ([]string, error) {
	if err := v.Available(); err != nil {
		return nil, err
	}
	var sealed []string
	for _, e := range v.Entries() {
		plain, err := os.ReadFile(e.Plain)
		if err != nil {
			continue // nothing to seal for this entry
		}
		if _, err := os.Stat(e.Enc); err == nil {
			if current, err := v.decrypt(ctx, e.Enc); err == nil && bytes.Equal(current, plain) {
				continue // unchanged — skip to keep git clean
			}
		}
		if err := os.MkdirAll(filepath.Dir(e.Enc), 0755); err != nil {
			return sealed, fmt.Errorf("create vault dir: %w", err)
		}
		if err := v.encrypt(ctx, e.Plain, e.Enc); err != nil {
			return sealed, fmt.Errorf("encrypt %s: %w", e.Plain, err)
		}
		v.logger.Info("sealed secret into vault", zap.String("plain", e.Plain), zap.String("enc", e.Enc))
		sealed = append(sealed, e.Enc)
	}
	return sealed, nil
}

// Lock seals the vault and then removes the plaintext working copies. The
// encrypted vault remains as the source of truth; a subsequent Hydrate restores
// the plaintext. Note: removal is not a secure on-disk erase.
func (v *Vault) Lock(ctx context.Context) error {
	if _, err := v.Seal(ctx); err != nil {
		return err
	}
	return v.WipePlaintext()
}

// WipePlaintext removes the plaintext working copies without sealing. Callers
// must ensure the vault is already current. Note: this is a plain removal, not
// a secure on-disk erase.
func (v *Vault) WipePlaintext() error {
	for _, e := range v.Entries() {
		if err := os.Remove(e.Plain); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", e.Plain, err)
		}
		v.logger.Info("removed plaintext working copy", zap.String("plain", e.Plain))
	}
	return nil
}

// EntryStatus describes the state of a single vault entry for reporting.
type EntryStatus struct {
	Entry
	HasPlain bool
	HasEnc   bool
}

// Status reports the presence of plaintext and encrypted files for each entry.
func (v *Vault) Status() []EntryStatus {
	var out []EntryStatus
	for _, e := range v.Entries() {
		st := EntryStatus{Entry: e}
		if _, err := os.Stat(e.Plain); err == nil {
			st.HasPlain = true
		}
		if _, err := os.Stat(e.Enc); err == nil {
			st.HasEnc = true
		}
		out = append(out, st)
	}
	return out
}

// decrypt returns the plaintext bytes of an encrypted vault file. The decrypted
// content is never logged.
func (v *Vault) decrypt(ctx context.Context, enc string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, v.sops, "decrypt", "--output-type", "binary", enc)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sops decrypt: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// encrypt writes an encrypted vault file from a plaintext source. Source content
// is treated as opaque binary so any file format (HCL, YAML, JSON) round-trips
// byte-for-byte. Creation rules come from the repo-root .sops.yaml, matched via
// the encrypted filename.
func (v *Vault) encrypt(ctx context.Context, plain, enc string) error {
	cmd := exec.CommandContext(ctx, v.sops, "encrypt",
		"--input-type", "binary", "--output-type", "yaml",
		"--filename-override", enc, plain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sops encrypt: %w: %s", err, stderr.String())
	}
	return writeFileAtomic(enc, stdout.Bytes(), 0644)
}

// updateKeys re-encrypts the data key of a vault file to match the recipients in
// .sops.yaml. Used after the recipient set changes.
func (v *Vault) updateKeys(ctx context.Context, enc string) error {
	cmd := exec.CommandContext(ctx, v.sops, "updatekeys", "-y", enc)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sops updatekeys %s: %w: %s", enc, err, stderr.String())
	}
	return nil
}

// writeFileAtomic writes data to path via a temp file and rename.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
