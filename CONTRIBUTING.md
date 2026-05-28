# Contributing

## Commit Convention

This repository follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).

### Types

| Type | When to use |
|------|-------------|
| `feat` | New infrastructure capability (new cluster, new module) |
| `fix` | Bug fix in Terraform config or bootstrap scripts |
| `chore` | Maintenance: provider upgrades, config cleanup |
| `docs` | Documentation only (no infra changes) |
| `ci` | CI/CD pipeline changes |
| `refactor` | Restructure with no functional change |
| `test` | Adding or updating infrastructure tests |
| `revert` | Reverting a previous commit |

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
chore: upgrade proxmox provider to 3.0.1
docs: add scenario for node replacement runbook
ci: add terraform validate to PR workflow
```

### Rules

- Subject line ≤72 characters, lowercase, no trailing period
- Use imperative mood: "add" not "added" / "adds"
- Breaking changes: add `!` after type/scope and a `BREAKING CHANGE:` footer

## Pull Requests

1. Branch from `main`: `git checkout -b feat/short-description`
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
