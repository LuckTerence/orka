/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func namedHost(name, uid string) *corev1alpha1.AgentHost {
	return &corev1alpha1.AgentHost{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", UID: types.UID(uid)}}
}

func TestObjectNamesKeepAShortNameInTheConnectionSecret(t *testing.T) {
	if got := ObjectNames(namedHost("alice", "uid-1"), "orka-agent-hosts").Connection; got != "alice-connection" {
		t.Fatalf("connection Secret = %q", got)
	}
}

func TestObjectNamesStayValidAndDistinctForLongNames(t *testing.T) {
	// A 253-character name whose 242nd character is a dot.
	long := strings.Repeat("a", 241) + "." + strings.Repeat("b", 11)
	other := strings.Repeat("a", 241) + "." + strings.Repeat("c", 11)
	first := ObjectNames(namedHost(long, "uid-1"), "orka-agent-hosts")
	second := ObjectNames(namedHost(other, "uid-2"), "orka-agent-hosts")
	for _, names := range []Names{first, second} {
		if errs := validation.IsDNS1123Subdomain(names.Connection); len(errs) != 0 {
			t.Fatalf("connection Secret %q is invalid: %v", names.Connection, errs)
		}
	}
	if first.Connection == second.Connection {
		t.Fatalf("two hosts share the connection Secret %q", first.Connection)
	}
}

func TestOwnerLabelsStayValidForLongNames(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 62) + "-b", strings.Repeat("a", 62) + ".b"} {
		value := OwnerLabels(namedHost(name, "uid-1"))[LabelHostName]
		if errs := validation.IsValidLabelValue(value); len(errs) != 0 {
			t.Fatalf("host name label %q is invalid: %v", value, errs)
		}
	}
}
