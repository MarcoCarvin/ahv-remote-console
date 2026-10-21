package presence

import "encoding/json"

type MessageType string

const (
	MsgJoin      MessageType = "presence.join"
	MsgLeave     MessageType = "presence.leave"
	MsgHeartbeat MessageType = "presence.heartbeat"
	MsgCursor    MessageType = "presence.cursor"
	MsgRoleChange MessageType = "presence.role_change"
	MsgRoomState MessageType = "presence.room_state"
	MsgChat      MessageType = "presence.chat"
	MsgChatHistory MessageType = "presence.chat_history"
)

type Message struct {
	Type      MessageType     `json:"type"`
	SessionID string          `json:"session_id"`
	VMUUID    string          `json:"vm_uuid,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type JoinPayload struct {
	Name     string          `json:"name"`
	Color    string          `json:"color"`
	Role     Role            `json:"role"`
	Viewport Viewport        `json:"viewport"`
	Identity SessionIdentity `json:"identity"`
}

type LeavePayload struct {
	Reason string `json:"reason,omitempty"`
}

type HeartbeatPayload struct {
	Cursor   CursorPosition `json:"cursor"`
	Viewport Viewport       `json:"viewport"`
}

type CursorPayload struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type RoleChangePayload struct {
	Role Role   `json:"role"`
	From string `json:"from,omitempty"`
}

type RoomStatePayload struct {
	Peers []Peer `json:"peers"`
}

type ChatPayload struct {
	Text string `json:"text"`
	Ts   int64  `json:"ts"`
	Name string `json:"name,omitempty"`
	Color string `json:"color,omitempty"`
}

type ChatHistoryPayload struct {
	Messages []ChatMessage `json:"messages"`
}

type ChatMessage struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Text      string `json:"text"`
	Ts        int64  `json:"ts"`
}
