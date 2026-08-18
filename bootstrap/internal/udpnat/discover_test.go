package udpnat

import (
	"strings"
	"testing"
)

const svcJSON = `{"items":[
 {"metadata":{"name":"minecraft","namespace":"jdwillmsen-prd","annotations":{"platform.jdwlabs.io/udp-external-port":"19132"}},
  "spec":{"type":"NodePort","ports":[{"protocol":"UDP","nodePort":31132}]}},
 {"metadata":{"name":"unrelated","namespace":"default","annotations":{}},
  "spec":{"type":"ClusterIP","ports":[{"protocol":"TCP","nodePort":0}]}}
]}`

func TestForwardsFromServicesPicksAnnotatedOnly(t *testing.T) {
	got, err := ForwardsFromServices([]byte(svcJSON))
	if err != nil {
		t.Fatalf("ForwardsFromServices: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 forward, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Name != "jdwillmsen-prd/minecraft" || f.ExternalPort != 19132 || f.NodePort != 31132 {
		t.Errorf("unexpected forward: %+v", f)
	}
}

// An annotated service that cannot be published must fail loudly. Skipping it
// would surface as "the port forward silently does nothing", which is the
// failure mode this package exists to avoid.
func TestForwardsFromServicesRejectsUnusableAnnotated(t *testing.T) {
	cases := map[string]string{
		"not a NodePort": `{"items":[{"metadata":{"name":"a","namespace":"n","annotations":{"platform.jdwlabs.io/udp-external-port":"19132"}},"spec":{"type":"ClusterIP","ports":[{"protocol":"UDP","nodePort":0}]}}]}`,
		"no UDP port":    `{"items":[{"metadata":{"name":"a","namespace":"n","annotations":{"platform.jdwlabs.io/udp-external-port":"19132"}},"spec":{"type":"NodePort","ports":[{"protocol":"TCP","nodePort":31132}]}}]}`,
		"two UDP ports":  `{"items":[{"metadata":{"name":"a","namespace":"n","annotations":{"platform.jdwlabs.io/udp-external-port":"19132"}},"spec":{"type":"NodePort","ports":[{"protocol":"UDP","nodePort":31132},{"protocol":"UDP","nodePort":31133}]}}]}`,
		"non-numeric":    `{"items":[{"metadata":{"name":"a","namespace":"n","annotations":{"platform.jdwlabs.io/udp-external-port":"nineteen"}},"spec":{"type":"NodePort","ports":[{"protocol":"UDP","nodePort":31132}]}}]}`,
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ForwardsFromServices([]byte(js)); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestForwardsFromServicesRejectsMalformedJSON(t *testing.T) {
	if _, err := ForwardsFromServices([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "parsing service list") {
		t.Errorf("unhelpful error: %v", err)
	}
}
