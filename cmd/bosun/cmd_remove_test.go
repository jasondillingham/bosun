package main

import (
	"strings"
	"testing"
)

// TestLiveAgentRemoveMessage pins the v0.6 liveness-gate refusal message:
// label, pid, recovery hint, and --ignore-running escape hatch must all
// appear so the operator never has to grep our docs to recover. Brittle
// on purpose — operator muscle memory anchors on this exact shape.
func TestLiveAgentRemoveMessage(t *testing.T) {
	got := liveAgentRemoveMessage("session-2", 12345)
	for _, want := range []string{
		"session-2",
		"pid 12345",
		"live agent",
		"uncommitted changes",
		"refusing remove",
		"let the agent finish",
		"--ignore-running",
		"discards uncommitted work",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("liveAgentRemoveMessage missing %q\n--- got ---\n%s", want, got)
		}
	}
}
