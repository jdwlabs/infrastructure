package app

import (
	"fmt"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/haproxy"
)

// The help lines below name the specific command that moves the caller forward
// from the state just reported, rather than pointing at --help. They are
// suggestions for discovery, not a fixed sequence: a caller that already knows
// what it wants should never be nudged through an extra step.

func helpForFailure(f *haproxy.Failure) []string {
	if f == nil {
		return nil
	}
	switch f.Code {
	case "haproxy_host_unset":
		return []string{
			"Pass the address directly: talops haproxy status --host <ip>",
			"Or set haproxy_ip in the vaulted tfvars and re-run",
		}
	case "tfvars_not_found", "tfvars_unreadable":
		return []string{
			"Check the vault is hydrated: talops secrets status",
			"Point at the file explicitly: talops --tfvars <path> haproxy status",
		}
	case "state_unreadable":
		return []string{"Inspect cluster state: talops status"}
	case "no_control_planes":
		return []string{
			"Reconcile the cluster so control planes are recorded: talops reconcile --plan",
		}
	case "ssh_auth_unconfigured":
		return []string{
			"talops haproxy status --haproxy-ssh-key <path>",
			"Confirm the login user matches the host: talops --haproxy-user <user> haproxy status",
		}
	case "ssh_unreachable":
		return []string{
			"Verify the address answers before trusting the rest of this report",
			"A replacement on a temporary address: talops haproxy status --host <temp-ip>",
			"Confirm the login user exists on the host: talops --haproxy-user <user> haproxy status",
		}
	case "config_unreadable":
		return []string{
			"Confirm the login user can sudo: talops --haproxy-user <user> haproxy plan",
			"Check the service is installed at all: talops haproxy status",
		}
	case "push_failed":
		return []string{
			"The install path validates and rolls back, so the previous config is still serving",
			"Review what would change before retrying: talops haproxy plan --full",
		}
	case "unknown_field":
		return []string{"Drop --fields for the default schema, or use --full for every column"}
	}
	return nil
}

func statusHelp(res haproxy.StatusResult, vm haproxy.VMInfo) []string {
	var help []string

	switch res.ConfigDrift {
	case "true":
		help = append(help, "talops haproxy plan  # review the config change before pushing it")
	case haproxy.Unknown:
		help = append(help, "talops haproxy plan  # render the config and compare it against the host")
	}

	if len(res.Backends) > 0 && haproxy.UpCount(res.Backends) < len(res.Backends) {
		help = append(help,
			"A backend is down: talosctl -n <cp-ip> service etcd status  # check the node itself, not the load balancer")
	}

	help = append(help, vmHelp(vm)...)
	return help
}

func vmHelp(vm haproxy.VMInfo) []string {
	switch vm.Source {
	case "unmanaged":
		return []string{
			"This VM is not in Terraform: scenarios/haproxy-vm-rebuild.md replaces it with a declared one",
		}
	case "declared":
		return []string{
			"talops infra plan  # the VM is declared but not applied; review the plan, then a human applies it",
		}
	}
	return nil
}

func planHelp(res haproxy.PlanResult) []string {
	if !res.Drift {
		return []string{"talops haproxy status  # backend health, which a converged config does not prove"}
	}
	help := []string{"talops haproxy apply  # push this config (validated, rolled back on failure)"}
	if res.Truncated {
		help = append(help, fmt.Sprintf("talops haproxy plan --full  # the whole %d-char diff", res.DiffBytes))
	}
	return help
}

func applyHelp(res haproxy.ApplyResult) []string {
	if res.DryRun && res.Drift {
		return []string{"talops haproxy apply  # without --dry-run, to push the change"}
	}
	if res.Changed {
		return []string{"talops haproxy status  # confirm every backend came back up after the reload"}
	}
	return nil
}
