package udpnat

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// AnnotationExternalPort opts a Service into UDP publishing and declares which
// external port it claims. Opt-in rather than "every UDP NodePort": publishing
// a service to the internet should be a deliberate, reviewable line in the
// service's own manifest, not a side effect of its type.
const AnnotationExternalPort = "platform.jdwlabs.io/udp-external-port"

// serviceList is the subset of `kubectl get svc -A -o json` this package reads.
type serviceList struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Type  string `json:"type"`
			Ports []struct {
				Protocol string `json:"protocol"`
				NodePort int    `json:"nodePort"`
			} `json:"ports"`
		} `json:"spec"`
	} `json:"items"`
}

// ForwardsFromServices extracts forwards from `kubectl get svc -A -o json`.
//
// A Service qualifies when it carries the annotation, allocates a NodePort, and
// exposes exactly one UDP port. Anything annotated but unusable is an error
// rather than a skip: the operator asked for it to be published, so silently
// omitting it would present as "the port forward mysteriously does nothing".
// One unusable service therefore fails the whole render — see the package doc
// for the obligation that places on the caller.
func ForwardsFromServices(raw []byte) ([]Forward, error) {
	var list serviceList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("udpnat: parsing service list: %w", err)
	}

	var forwards []Forward
	for _, svc := range list.Items {
		val, ok := svc.Metadata.Annotations[AnnotationExternalPort]
		if !ok {
			continue
		}
		id := svc.Metadata.Namespace + "/" + svc.Metadata.Name

		external, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("udpnat: %s has non-numeric %s %q", id, AnnotationExternalPort, val)
		}

		// LoadBalancer allocates a NodePort too, so it is publishable by the
		// same rules; only types that never get one are rejected.
		if svc.Spec.Type != "NodePort" && svc.Spec.Type != "LoadBalancer" {
			return nil, fmt.Errorf("udpnat: %s is annotated for UDP publishing but is type %s, which allocates no NodePort",
				id, svc.Spec.Type)
		}

		var udp []int
		for _, p := range svc.Spec.Ports {
			if p.Protocol == "UDP" && p.NodePort != 0 {
				udp = append(udp, p.NodePort)
			}
		}
		switch len(udp) {
		case 1:
		case 0:
			return nil, fmt.Errorf("udpnat: %s is annotated for UDP publishing but exposes no UDP NodePort", id)
		default:
			// The annotation names one external port, so one UDP port is the
			// only unambiguous mapping.
			return nil, fmt.Errorf("udpnat: %s exposes %d UDP NodePorts; expected exactly one", id, len(udp))
		}

		forwards = append(forwards, Forward{
			Name:         id,
			ExternalPort: external,
			NodePort:     udp[0],
		})
	}
	return forwards, nil
}
