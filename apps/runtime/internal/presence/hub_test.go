package presence

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func testHub(t *testing.T) *Hub {
	t.Helper()
	h := NewHub(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	go h.Run()
	t.Cleanup(h.Stop)
	return h
}

func joinMsg(sessionID, vmUUID, name string) []byte {
	payload, _ := json.Marshal(JoinPayload{
		Name:  name,
		Color: "#d6552a",
		Identity: SessionIdentity{
			SessionID:         sessionID,
			UserID:            "user-" + sessionID,
			VMUUID:            vmUUID,
			RuntimeInstanceID: "rt-1",
		},
		Viewport: Viewport{W: 1920, H: 1080},
	})
	msg, _ := json.Marshal(Message{
		Type:      MsgJoin,
		SessionID: sessionID,
		VMUUID:    vmUUID,
		Payload:   payload,
	})
	return msg
}

func leaveMsg(sessionID, vmUUID string) []byte {
	payload, _ := json.Marshal(LeavePayload{Reason: "normal"})
	msg, _ := json.Marshal(Message{
		Type:      MsgLeave,
		SessionID: sessionID,
		VMUUID:    vmUUID,
		Payload:   payload,
	})
	return msg
}

func TestHub_JoinAndRoomState(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)

	// subA should get room state (sent to joiner)
	// subB should get the join broadcast
	select {
	case raw := <-subB.Send():
		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != MsgJoin {
			t.Fatalf("expected join, got %s", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for join broadcast")
	}
}

func TestHub_FirstPeerGetsController(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)

	peers := h.RoomPeers(vm)
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	if peers[0].Role != RoleController {
		t.Fatalf("first peer should be controller, got %s", peers[0].Role)
	}
}

func TestHub_SecondPeerGetsViewer(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	h.HandleMessage(joinMsg("sess-b", vm, "bob"), subB)

	peers := h.RoomPeers(vm)
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
	for _, p := range peers {
		if p.Identity.SessionID == "sess-a" && p.Role != RoleController {
			t.Fatalf("alice should be controller")
		}
		if p.Identity.SessionID == "sess-b" && p.Role != RoleViewer {
			t.Fatalf("bob should be viewer")
		}
	}
}

func TestHub_LeaveRemovesPeer(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	h.HandleMessage(leaveMsg("sess-a", vm), subA)

	peers := h.RoomPeers(vm)
	if len(peers) != 0 {
		t.Fatalf("expected 0 peers after leave, got %d", len(peers))
	}
}

func TestHub_RemovePeerBroadcastsLeave(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	h.HandleMessage(joinMsg("sess-b", vm, "bob"), subB)

	// drain join broadcasts
	drain(subA)
	drain(subB)

	h.RemovePeer("sess-a", vm)

	select {
	case raw := <-subB.Send():
		var msg Message
		_ = json.Unmarshal(raw, &msg)
		if msg.Type != MsgLeave {
			t.Fatalf("expected leave, got %s", msg.Type)
		}
		if msg.SessionID != "sess-a" {
			t.Fatalf("expected sess-a leave, got %s", msg.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for leave broadcast")
	}
}

func TestHub_CursorRateLimit(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	drain(subB)

	cursorPayload, _ := json.Marshal(CursorPayload{X: 0.5, Y: 0.5})
	cursorMsg, _ := json.Marshal(Message{
		Type:    MsgCursor,
		VMUUID:  vm,
		Payload: cursorPayload,
	})

	// First cursor should go through
	h.HandleMessage(cursorMsg, subA)
	select {
	case <-subB.Send():
		// ok
	case <-time.After(time.Second):
		t.Fatal("first cursor should be delivered")
	}

	// Immediately following cursor should be rate-limited
	h.HandleMessage(cursorMsg, subA)
	select {
	case <-subB.Send():
		t.Fatal("second cursor should be rate-limited")
	case <-time.After(50 * time.Millisecond):
		// ok, was dropped
	}
}

func TestHub_RoleChange(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	h.HandleMessage(joinMsg("sess-b", vm, "bob"), subB)
	drain(subA)
	drain(subB)

	rolePayload, _ := json.Marshal(RoleChangePayload{Role: RoleController})
	roleMsg, _ := json.Marshal(Message{
		Type:      MsgRoleChange,
		SessionID: "sess-b",
		VMUUID:    vm,
		Payload:   rolePayload,
	})
	h.HandleMessage(roleMsg, subB)

	peers := h.RoomPeers(vm)
	for _, p := range peers {
		if p.Identity.SessionID == "sess-a" && p.Role != RoleViewer {
			t.Fatalf("alice should be demoted to viewer, got %s", p.Role)
		}
		if p.Identity.SessionID == "sess-b" && p.Role != RoleController {
			t.Fatalf("bob should be promoted to controller, got %s", p.Role)
		}
	}
}

func TestHub_Chat(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	h.HandleMessage(joinMsg("sess-b", vm, "bob"), subB)
	drain(subA)
	drain(subB)

	chatPayload, _ := json.Marshal(ChatPayload{Text: "hello world", Ts: 1700000000000})
	chatMsg, _ := json.Marshal(Message{
		Type:    MsgChat,
		VMUUID:  vm,
		Payload: chatPayload,
	})
	h.HandleMessage(chatMsg, subA)

	// Both sender and receiver should get the chat (broadcastIncludingSender)
	received := 0
	for i := 0; i < 2; i++ {
		select {
		case raw := <-subA.Send():
			var msg Message
			_ = json.Unmarshal(raw, &msg)
			if msg.Type == MsgChat {
				received++
			}
		case raw := <-subB.Send():
			var msg Message
			_ = json.Unmarshal(raw, &msg)
			if msg.Type == MsgChat {
				received++
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for chat")
		}
	}
	if received != 2 {
		t.Fatalf("expected 2 chat deliveries, got %d", received)
	}
}

func TestHub_ChatRateLimit(t *testing.T) {
	h := testHub(t)
	vm := "vm-001"

	subA := h.Subscribe("sess-a", vm)
	defer h.Unsubscribe(subA)
	subB := h.Subscribe("sess-b", vm)
	defer h.Unsubscribe(subB)

	h.HandleMessage(joinMsg("sess-a", vm, "alice"), subA)
	h.HandleMessage(joinMsg("sess-b", vm, "bob"), subB)
	drain(subA)
	drain(subB)

	// Send 7 messages rapidly — only first 5 should go through
	for i := 0; i < 7; i++ {
		chatPayload, _ := json.Marshal(ChatPayload{Text: "spam", Ts: int64(i)})
		chatMsg, _ := json.Marshal(Message{
			Type:    MsgChat,
			VMUUID:  vm,
			Payload: chatPayload,
		})
		h.HandleMessage(chatMsg, subA)
	}

	delivered := 0
	for {
		select {
		case <-subB.Send():
			delivered++
		case <-time.After(50 * time.Millisecond):
			goto done
		}
	}
done:
	if delivered != 5 {
		t.Fatalf("expected 5 messages through rate limit, got %d", delivered)
	}
}

// TestHub_StopDuringJoinNoPanic guards GO-R-01: a join arriving on the
// presence reader goroutine while Stop() is closing every sub.send must not
// race into a send-on-closed-channel panic. sendRoomState/sendChatHistory now
// send while holding the read lock, so Stop()'s write-locked close() cannot
// close the channel mid-send. Run with -race to catch the data race too.
func TestHub_StopDuringJoinNoPanic(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		h := NewHub(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
		go h.Run()
		vm := "vm-001"

		// Seed a peer + chat backlog so sendRoomState/sendChatHistory both
		// have data to deliver to the joining subscriber.
		seed := h.Subscribe("sess-seed", vm)
		h.HandleMessage(joinMsg("sess-seed", vm, "seed"), seed)
		chatPayload, _ := json.Marshal(ChatPayload{Text: "history", Ts: 1})
		chatMsg, _ := json.Marshal(Message{Type: MsgChat, VMUUID: vm, Payload: chatPayload})
		h.HandleMessage(chatMsg, seed)

		joiner := h.Subscribe("sess-join", vm)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Mirrors the unrecovered presence reader goroutine handling a
			// MsgJoin: this drives sendRoomState + sendChatHistory.
			h.HandleMessage(joinMsg("sess-join", vm, "joiner"), joiner)
		}()

		// Stop concurrently — closes joiner.send under the write lock.
		h.Stop()
		wg.Wait()
	}
}

func drain(sub *subscriber) {
	for {
		select {
		case <-sub.Send():
		default:
			return
		}
	}
}
