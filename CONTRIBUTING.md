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
| `Refs: JDWLABS-XX` | Links commit to a Jira issue |
| `Refs: #N` | Links commit to a GitHub issue by number |
| `Closes: #N` | Auto-closes a GitHub issue on merge |
| `BREAKING CHANGE: <desc>` | Required when a commit changes cluster topology or removes a module interface |
| `Co-Authored-By: Name <email>` | Credit a co-author (human or AI) |

**AI contributor footer** — include when commits were written with AI assistance:

```
Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
```

**Full examples with footers:**

```
feat(clusters): add talos-prod-2 worker node pool

Adds 3 additional worker nodes to the production cluster.
Terraform plan output reviewed and approved before apply.

Refs: JDWLABS-71
Co-Authored-By: Claude Sonnet 4.6 <noreply@anthropic.com>
```

```
feat(terraform)!: remove proxmox-v2 module

BREAKING CHANGE: proxmox-v2 module removed; all clusters must
migrate to proxmox-v3 before applying this change.

Refs: JDWLABS-68
```

### Rules

- Subject line ≤72 characters, lowercase, no trailing period
- Use imperative mood: "add" not "added" / "adds"
- Breaking changes: add `!` after type/scope and a `BREAKING CHANGE:` footer

## Pull Requests

1. Create a worktree: `gwta feat/short-description` (or `git worktree add ~/worktrees/infrastructure/feat/short-description -b feat/short-description`)
2. Run `terraform validate` before opening PR
3. PR title must follow conventional commit format
4. Include `terraform plan` output in PR description for any infra changes
5. Squash-merge to main

## Development Setup

```bash
terraform init                    # Initialize (once per working dir)
terraform validate                # Validate config
terraform plan -out=tfplan        # Preview changes
```

Terraform state is remote (S3-compatible MinIO), so `init` and `plan` need the
backend credentials from `terraform/backend-credentials.enc.yaml` — see
[docs/secrets.md](docs/secrets.md).
