package presence

import "time"

type SessionIdentity struct {
	SessionID         string `json:"session_id"`
	UserID            string `json:"user_id"`
	VMUUID            string `json:"vm_uuid"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
}

type Role string

const (
	RoleController Role = "controller"
	RoleViewer     Role = "viewer"
)

type Peer struct {
	Identity  SessionIdentity `json:"identity"`
	Name      string          `json:"name"`
	Color     string          `json:"color"`
	Role      Role            `json:"role"`
	Cursor    CursorPosition  `json:"cursor"`
	Viewport  Viewport        `json:"viewport"`
	JoinedAt  time.Time       `json:"joined_at"`
	LastSeen  time.Time       `json:"last_seen"`
}

type CursorPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Viewport struct {
	W int `json:"w"`
	H int `json:"h"`
}
