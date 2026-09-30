# Contributing

## Commit Convention

This repository follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).

### Types

| Type | When to use |
|------|-------------|
| `feat` | New infrastructure capability (new cluster, new module) |
| `fix` | Bug fix in Terraform config or bootstrap scripts |
| `build` | Provider, module, or tool version change |
| `chore` | Maintenance: config cleanup, tooling (no infra change) |
| `ci` | CI/CD pipeline changes |
| `docs` | Documentation only (no infra changes) |
| `perf` | Performance improvement |
| `refactor` | Restructure with no functional change |
| `revert` | Reverting a previous commit |
| `style` | Formatting or whitespace only (no logic change) |
| `test` | Adding or updating infrastructure tests |

### Format

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

### Examples

```
feat(clusters): add talos-prod cluster definition
fix(terraform): correct worker node count for talos-4h8
build: upgrade proxmox provider to 3.0.1
docs: add scenario for node replacement runbook
ci: add terraform validate to PR workflow
```

### Footers

Footers appear after an optional body, separated by a blank line. Common footers:

| Footer | When to use |
|--------|-------------|
| `Refs: KEY-123` | Links commit to a Jira issue |
| `Refs: #N` | Links commit to a GitHub issue by number |
| `Closes: #N` | Auto-closes a GitHub issue on merge |
| `BREAKING CHANGE: <desc>` | Required when a commit changes cluster topology or removes a module interface |
| `Co-Authored-By: Name <email>` | Credit a co-author (human or AI) |

**AI attribution** — every AI-assisted commit names the agent and the model that actually ran, in two trailers:

```
Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Assisted-by: Claude Code:claude-opus-5-5
```

Codex: `Co-Authored-By: Codex <codex@openai.com>` and `Assisted-by: Codex:<model-id>`. Attribution belongs in commits only, never in PR titles, bodies or comments.

**Full example with footers:**

```
feat(terraform)!: remove proxmox-v2 module

BREAKING CHANGE: proxmox-v2 module removed; all clusters must
migrate to proxmox-v3 before applying this change.

Refs: #68
```

### Rules

- Subject line ≤72 characters, lowercase, no trailing period
- Use imperative mood: "add" not "added" / "adds"
- Breaking changes: add `!` after type/scope and a `BREAKING CHANGE:` footer

## Pull Requests

1. Work in a worktree on a feature branch: `gwta feat/short-description` (or `git worktree add ~/worktrees/infrastructure/feat/short-description -b feat/short-description`)
2. Run `terraform validate` before opening the PR
3. PR title follows conventional commit format, under 70 characters
4. Description follows the template: keep only sections with content, ~150 words. For infra changes put the one-line `terraform plan` summary (N to add / change / destroy) under **Verified** — not the full plan
5. Rebase-merge to main (squash and merge commits are disabled), so every commit must stand alone

## Development Setup

```bash
terraform init                    # Initialize (once per working dir)
terraform validate                # Validate config
terraform plan -out=tfplan        # Preview changes
```

Terraform state is remote (S3-compatible MinIO), so `init` and `plan` need the
backend credentials from `terraform/backend-credentials.enc.yaml` — see
[docs/secrets.md](docs/secrets.md).
