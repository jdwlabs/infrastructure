package haproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
)

// Instance is one load-balancer host the config is installed on.
type Instance struct {
	// Name is the declared VM name, empty when the address matches no
	// haproxy_vms entry.
	Name string `json:"name,omitempty"`
	Host string `json:"host"`
}

// Label names the instance for a human: its VM name when it has one.
func (i Instance) Label() string {
	if i.Name != "" {
		return i.Name
	}
	return i.Host
}

// Group is where the rendered config goes and which address it binds.
//
// Two or more declared VMs are a keepalived VRRP group: haproxy_ip floats
// between them, so every instance binds it and each is reached on its own
// address. Pushing to the floating address instead would reach only whichever
// instance holds it and leave the rest serving the old config.
type Group struct {
	BindAddress net.IP
	Instances   []Instance
	// Virtual is true when BindAddress floats between Instances.
	Virtual bool
}

// ResolveGroup works out the push targets from tfvars. hostOverride narrows it
// to one address, for staging a member or verifying a replacement.
func ResolveGroup(cfg *types.Config, hostOverride string) (Group, error) {
	grouped := len(cfg.HAProxyVMs) > 1

	if hostOverride != "" {
		ip := net.ParseIP(hostOverride)
		if ip == nil {
			return Group{}, fmt.Errorf("%q is not an IP address", hostOverride)
		}
		inst := Instance{Name: declaredName(cfg, hostOverride), Host: hostOverride}
		if grouped && inst.Name != "" && cfg.HAProxyIP != nil {
			return Group{BindAddress: cfg.HAProxyIP, Instances: []Instance{inst}}, nil
		}
		return Group{BindAddress: ip, Instances: []Instance{inst}}, nil
	}

	if cfg.HAProxyIP == nil {
		return Group{}, errors.New("haproxy_ip is not set")
	}
	endpoint := cfg.HAProxyIP.String()

	if !grouped {
		return Group{
			BindAddress: cfg.HAProxyIP,
			Instances:   []Instance{{Name: declaredName(cfg, endpoint), Host: endpoint}},
		}, nil
	}

	group := Group{BindAddress: cfg.HAProxyIP, Virtual: true}
	for _, vm := range cfg.HAProxyVMs {
		if net.ParseIP(vm.IP) == nil {
			return Group{}, fmt.Errorf("haproxy_vms entry %s has no usable ip (%q)", vm.Name, vm.IP)
		}
		if vm.IP == endpoint {
			return Group{}, fmt.Errorf(
				"haproxy_vms entry %s uses %s, which is the virtual address (haproxy_ip): each instance needs its own",
				vm.Name, vm.IP)
		}
		group.Instances = append(group.Instances, Instance{Name: vm.Name, Host: vm.IP})
	}
	return group, nil
}

func declaredName(cfg *types.Config, host string) string {
	for _, vm := range cfg.HAProxyVMs {
		if vm.IP == host {
			return vm.Name
		}
	}
	return ""
}

// Pusher installs a rendered config on one instance.
type Pusher interface {
	Update(ctx context.Context, config string) error
}

// PushOutcome is what happened on one instance.
type PushOutcome struct {
	Instance Instance
	Err      error
}

// PushToAll installs config on every instance and never stops at a failure.
// An unreachable instance is the case a group exists to survive, and the one
// still serving is exactly the one that must receive the new backends.
func PushToAll(
	ctx context.Context,
	instances []Instance,
	connect func(Instance) (Pusher, error),
	config string,
) []PushOutcome {
	outcomes := make([]PushOutcome, 0, len(instances))
	for _, inst := range instances {
		pusher, err := connect(inst)
		if err == nil {
			err = pusher.Update(ctx, config)
		}
		outcomes = append(outcomes, PushOutcome{Instance: inst, Err: err})
	}
	return outcomes
}

// DivergenceError reports a push that reached some instances and not others,
// leaving them serving different configs. It is distinct from a push that
// failed everywhere: a caller mid-way through a larger operation can carry on
// with a divergent group, because the instances that took the config are
// serving it, but must still surface the divergence.
type DivergenceError struct {
	Updated []string
	Failed  []string
	causes  []error
}

func (e *DivergenceError) Error() string {
	details := make([]string, 0, len(e.Failed))
	for i, name := range e.Failed {
		details = append(details, fmt.Sprintf("%s: %v", name, e.causes[i]))
	}
	return fmt.Sprintf(
		"HAProxy config pushed to %s but not to %s — the instances are serving different configs until `talops haproxy apply` succeeds on all of them (%s)",
		strings.Join(e.Updated, ", "), strings.Join(e.Failed, ", "), strings.Join(details, "; "))
}

func (e *DivergenceError) Unwrap() []error { return e.causes }

// PushError reduces per-instance outcomes to one error: nil when every push
// landed, a DivergenceError when only some did.
func PushError(outcomes []PushOutcome) error {
	var updated, failed []string
	var causes []error
	for _, o := range outcomes {
		if o.Err != nil {
			failed = append(failed, o.Instance.Label())
			causes = append(causes, o.Err)
			continue
		}
		updated = append(updated, o.Instance.Label())
	}

	switch {
	case len(failed) == 0:
		return nil
	case len(updated) > 0:
		return &DivergenceError{Updated: updated, Failed: failed, causes: causes}
	case len(outcomes) == 1:
		return causes[0]
	}

	details := make([]string, 0, len(failed))
	for i, name := range failed {
		details = append(details, fmt.Sprintf("%s: %v", name, causes[i]))
	}
	return fmt.Errorf("HAProxy config push failed on every instance (%s): %w",
		strings.Join(details, "; "), errors.Join(causes...))
}
