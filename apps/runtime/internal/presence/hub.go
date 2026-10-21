package presence

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

const (
	peerTTL         = 15 * time.Second
	reapInterval    = 5 * time.Second
	cursorRateHz    = 30
	cursorMinGap    = time.Second / time.Duration(cursorRateHz)
	chatBacklogSize = 200
	chatRateLimit   = 5 // messages per second per client
)

type subscriber struct {
	send       chan []byte
	sessionID  string
	vmUUID     string
	lastCursor time.Time
	chatCount  int
	chatWindow time.Time
}

type Hub struct {
	mu          sync.RWMutex
	rooms       map[string]*room // keyed by vm-uuid
	subscribers map[*subscriber]struct{}
	log         *slog.Logger
	stop        chan struct{}
	closed      bool // set under mu.Lock() in Stop(); guards sends to sub.send
}

type room struct {
	vmUUID      string
	peers       map[string]*Peer // keyed by session-id
	chatBacklog []ChatMessage
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		rooms:       make(map[string]*room),
		subscribers: make(map[*subscriber]struct{}),
		log:         log.With("component", "presence"),
		stop:        make(chan struct{}),
	}
}

func (h *Hub) Run() {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.reap()
		}
	}
}

func (h *Hub) Stop() {
	close(h.stop)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subscribers {
		close(sub.send)
		delete(h.subscribers, sub)
	}
}

func (h *Hub) Subscribe(sessionID, vmUUID string) *subscriber {
	sub := &subscriber{
		send:      make(chan []byte, 64),
		sessionID: sessionID,
		vmUUID:    vmUUID,
	}
	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *Hub) Unsubscribe(sub *subscriber) {
	h.mu.Lock()
	if _, ok := h.subscribers[sub]; ok {
		close(sub.send)
		delete(h.subscribers, sub)
	}
	h.mu.Unlock()
}

func (sub *subscriber) Send() <-chan []byte { return sub.send }

func (h *Hub) HandleMessage(raw []byte, from *subscriber) {
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		h.log.Debug("malformed presence message", "err", err)
		return
	}
	msg.SessionID = from.sessionID
	if msg.VMUUID == "" {
		msg.VMUUID = from.vmUUID
	}

	switch msg.Type {
	case MsgJoin:
		h.handleJoin(msg, from)
	case MsgLeave:
		h.handleLeave(msg, from)
	case MsgHeartbeat:
		h.handleHeartbeat(msg, from)
	case MsgCursor:
		h.handleCursor(msg, from)
	case MsgRoleChange:
		h.handleRoleChange(msg, from)
	case MsgChat:
		h.handleChat(msg, from)
	default:
		h.log.Debug("unknown presence message type", "type", msg.Type)
	}
}

func (h *Hub) handleJoin(msg Message, from *subscriber) {
	var payload JoinPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.log.Debug("malformed join payload", "err", err)
		return
	}

	h.mu.Lock()
	r := h.getOrCreateRoom(msg.VMUUID)
	role := payload.Role
	if role == "" {
		if len(r.peers) == 0 {
			role = RoleController
		} else {
			role = RoleViewer
		}
	}
	// If someone else already controls, force this peer to viewer
	if role == RoleController {
		for _, p := range r.peers {
			if p.Role == RoleController && p.Identity.SessionID != msg.SessionID {
				role = RoleViewer
				break
			}
		}
	}
	now := time.Now()
	r.peers[msg.SessionID] = &Peer{
		Identity: payload.Identity,
		Name:     payload.Name,
		Color:    payload.Color,
		Role:     role,
		Viewport: payload.Viewport,
		JoinedAt: now,
		LastSeen: now,
	}
	h.mu.Unlock()

	h.log.Info("peer joined", "session", msg.SessionID, "vm", msg.VMUUID,
		"name", payload.Name, "role", role)

	outPayload, _ := json.Marshal(JoinPayload{
		Name:     payload.Name,
		Color:    payload.Color,
		Role:     role,
		Viewport: payload.Viewport,
		Identity: payload.Identity,
	})
	h.broadcast(msg.VMUUID, Message{
		Type:      MsgJoin,
		SessionID: msg.SessionID,
		VMUUID:    msg.VMUUID,
		Payload:   outPayload,
	})

	h.sendRoomState(msg.VMUUID, from)
	h.sendChatHistory(msg.VMUUID, from)
}

func (h *Hub) handleLeave(msg Message, _ *subscriber) {
	h.mu.Lock()
	r, ok := h.rooms[msg.VMUUID]
	if ok {
		delete(r.peers, msg.SessionID)
		if len(r.peers) == 0 {
			delete(h.rooms, msg.VMUUID)
		}
	}
	h.mu.Unlock()

	h.log.Info("peer left", "session", msg.SessionID, "vm", msg.VMUUID)

	h.broadcast(msg.VMUUID, Message{
		Type:      MsgLeave,
		SessionID: msg.SessionID,
		VMUUID:    msg.VMUUID,
		Payload:   msg.Payload,
	})
}

func (h *Hub) handleHeartbeat(msg Message, from *subscriber) {
	var payload HeartbeatPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}

	h.mu.Lock()
	if r, ok := h.rooms[msg.VMUUID]; ok {
		if p, ok := r.peers[msg.SessionID]; ok {
			p.LastSeen = time.Now()
			p.Cursor = payload.Cursor
			p.Viewport = payload.Viewport
		}
	}
	h.mu.Unlock()
}

func (h *Hub) handleCursor(msg Message, from *subscriber) {
	now := time.Now()
	if now.Sub(from.lastCursor) < cursorMinGap {
		return
	}
	from.lastCursor = now

	h.mu.Lock()
	if r, ok := h.rooms[msg.VMUUID]; ok {
		if p, ok := r.peers[msg.SessionID]; ok {
			var payload CursorPayload
			if err := json.Unmarshal(msg.Payload, &payload); err == nil {
				p.Cursor = CursorPosition(payload)
				p.LastSeen = now
			}
		}
	}
	h.mu.Unlock()

	h.broadcast(msg.VMUUID, msg)
}

func (h *Hub) handleRoleChange(msg Message, _ *subscriber) {
	var payload RoleChangePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}

	h.mu.Lock()
	r, ok := h.rooms[msg.VMUUID]
	if !ok {
		h.mu.Unlock()
		return
	}

	targetPeer, ok := r.peers[msg.SessionID]
	if !ok {
		h.mu.Unlock()
		return
	}

	if payload.Role == RoleController {
		for sid, p := range r.peers {
			if p.Role == RoleController && sid != msg.SessionID {
				p.Role = RoleViewer
				demotePayload, _ := json.Marshal(RoleChangePayload{Role: RoleViewer, From: msg.SessionID})
				h.broadcastLocked(msg.VMUUID, Message{
					Type:      MsgRoleChange,
					SessionID: sid,
					VMUUID:    msg.VMUUID,
					Payload:   demotePayload,
				})
			}
		}
	}
	targetPeer.Role = payload.Role
	h.mu.Unlock()

	h.log.Info("role changed", "session", msg.SessionID, "role", payload.Role, "vm", msg.VMUUID)
	h.broadcast(msg.VMUUID, msg)
}

func (h *Hub) handleChat(msg Message, from *subscriber) {
	var payload ChatPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}
	if len(payload.Text) == 0 || len(payload.Text) > 2000 {
		return
	}

	now := time.Now()
	if now.Sub(from.chatWindow) > time.Second {
		from.chatCount = 0
		from.chatWindow = now
	}
	from.chatCount++
	if from.chatCount > chatRateLimit {
		return
	}

	h.mu.Lock()
	r, ok := h.rooms[msg.VMUUID]
	if !ok {
		h.mu.Unlock()
		return
	}
	peer, ok := r.peers[msg.SessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	cm := ChatMessage{
		SessionID: msg.SessionID,
		Name:      peer.Name,
		Color:     peer.Color,
		Text:      payload.Text,
		Ts:        now.UnixMilli(),
	}
	r.chatBacklog = append(r.chatBacklog, cm)
	if len(r.chatBacklog) > chatBacklogSize {
		r.chatBacklog = r.chatBacklog[len(r.chatBacklog)-chatBacklogSize:]
	}
	h.mu.Unlock()

	chatPayload, _ := json.Marshal(ChatPayload{
		Text:  payload.Text,
		Ts:    cm.Ts,
		Name:  cm.Name,
		Color: cm.Color,
	})
	h.broadcastIncludingSender(msg.VMUUID, Message{
		Type:      MsgChat,
		SessionID: msg.SessionID,
		VMUUID:    msg.VMUUID,
		Payload:   chatPayload,
	})
}

func (h *Hub) RemovePeer(sessionID, vmUUID string) {
	h.mu.Lock()
	r, ok := h.rooms[vmUUID]
	if ok {
		delete(r.peers, sessionID)
		if len(r.peers) == 0 {
			delete(h.rooms, vmUUID)
		}
	}
	h.mu.Unlock()

	leavePayload, _ := json.Marshal(LeavePayload{Reason: "disconnect"})
	h.broadcast(vmUUID, Message{
		Type:      MsgLeave,
		SessionID: sessionID,
		VMUUID:    vmUUID,
		Payload:   leavePayload,
	})
}

// RoleOf returns the presence role currently assigned to a console session.
// Peers are keyed by vm-uuid then session-id; the proxy only knows the
// session-id, so we scan every room for a peer with that session-id. A session
// not present in any room (single-operator mode, or before the renderer's
// presence.join) has no presence entry, so we return RoleController — this is
// the security-relevant default that preserves "lone operator = controller"
// when presence tracking has nothing to say about the session. Used by the
// live-stream proxy's RoleChecker to strip a viewer's input (AHV-02).
func (h *Hub) RoleOf(sessionID string) Role {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, r := range h.rooms {
		if p, ok := r.peers[sessionID]; ok {
			return p.Role
		}
	}
	return RoleController
}

func (h *Hub) RoomPeers(vmUUID string) []Peer {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[vmUUID]
	if !ok {
		return nil
	}
	out := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, *p)
	}
	return out
}

func (h *Hub) getOrCreateRoom(vmUUID string) *room {
	r, ok := h.rooms[vmUUID]
	if !ok {
		r = &room{vmUUID: vmUUID, peers: make(map[string]*Peer)}
		h.rooms[vmUUID] = r
	}
	return r
}

func (h *Hub) sendRoomState(vmUUID string, to *subscriber) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[vmUUID]
	if !ok {
		return
	}
	peers := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, *p)
	}

	payload, _ := json.Marshal(RoomStatePayload{Peers: peers})
	data, _ := json.Marshal(Message{
		Type:    MsgRoomState,
		VMUUID:  vmUUID,
		Payload: payload,
	})
	// Send while still holding the read lock and only if the hub is not
	// shutting down: Stop() sets closed and closes every sub.send under the
	// write lock, so checking closed here rules out a send-on-closed-channel
	// panic (the to subscriber is passed in, not re-read from the live map).
	if h.closed {
		return
	}
	select {
	case to.send <- data:
	default:
	}
}

func (h *Hub) sendChatHistory(vmUUID string, to *subscriber) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[vmUUID]
	if !ok || len(r.chatBacklog) == 0 {
		return
	}
	msgs := make([]ChatMessage, len(r.chatBacklog))
	copy(msgs, r.chatBacklog)

	payload, _ := json.Marshal(ChatHistoryPayload{Messages: msgs})
	data, _ := json.Marshal(Message{
		Type:    MsgChatHistory,
		VMUUID:  vmUUID,
		Payload: payload,
	})
	// Send while still holding the read lock and only if the hub is not
	// shutting down: Stop() sets closed and closes every sub.send under the
	// write lock, so checking closed here rules out a send-on-closed-channel
	// panic (the to subscriber is passed in, not re-read from the live map).
	if h.closed {
		return
	}
	select {
	case to.send <- data:
	default:
	}
}

func (h *Hub) broadcast(vmUUID string, msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subscribers {
		if sub.vmUUID != vmUUID || sub.sessionID == msg.SessionID {
			continue
		}
		select {
		case sub.send <- data:
		default:
			h.log.Debug("dropping presence message for slow subscriber", "session", sub.sessionID)
		}
	}
}

func (h *Hub) broadcastIncludingSender(vmUUID string, msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subscribers {
		if sub.vmUUID != vmUUID {
			continue
		}
		select {
		case sub.send <- data:
		default:
		}
	}
}

func (h *Hub) broadcastLocked(vmUUID string, msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	for sub := range h.subscribers {
		if sub.vmUUID != vmUUID || sub.sessionID == msg.SessionID {
			continue
		}
		select {
		case sub.send <- data:
		default:
		}
	}
}

func (h *Hub) reap() {
	deadline := time.Now().Add(-peerTTL)
	h.mu.Lock()
	defer h.mu.Unlock()
	for vmUUID, r := range h.rooms {
		for sid, p := range r.peers {
			if p.LastSeen.Before(deadline) {
				h.log.Info("reaping stale peer", "session", sid, "vm", vmUUID)
				delete(r.peers, sid)
				leavePayload, _ := json.Marshal(LeavePayload{Reason: "timeout"})
				h.broadcastLocked(vmUUID, Message{
					Type:      MsgLeave,
					SessionID: sid,
					VMUUID:    vmUUID,
					Payload:   leavePayload,
				})
			}
		}
		if len(r.peers) == 0 {
			delete(h.rooms, vmUUID)
		}
	}
}
