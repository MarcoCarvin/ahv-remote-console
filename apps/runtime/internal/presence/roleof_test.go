package presence

import "testing"

// TestHub_RoleOf backs the AHV-02 RoleChecker wiring: the proxy knows only the
// session-id, so RoleOf must find a joined peer's role across rooms, and return
// the controller default for a session with no presence entry (single-operator
// mode / before join).
func TestHub_RoleOf(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA) // first → controller
	h.HandleMessage(joinMsg("sess-b", vm, "bob"), subB)   // second → viewer

	if got := h.RoleOf("sess-a"); got != RoleController {
		t.Fatalf("RoleOf(sess-a) = %q, want controller", got)
	}
	if got := h.RoleOf("sess-b"); got != RoleViewer {
		t.Fatalf("RoleOf(sess-b) = %q, want viewer", got)
	}
	// A session not present in any room has no presence entry → controller
	// default, preserving lone-operator behavior.
	if got := h.RoleOf("sess-unknown"); got != RoleController {
		t.Fatalf("RoleOf(unknown) = %q, want controller default", got)
	}
}
