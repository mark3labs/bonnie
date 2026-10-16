package discoveryio

// ProtocolVersion identifies the discovery helper wire contract.
const ProtocolVersion = 1

// Response is a single bounded helper response. Error is a code, never file content.
type Response struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries,omitempty"`
	More    bool    `json:"more,omitempty"`
	File    *File   `json:"file,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// MaxProtocolBytes bounds serialized helper output (including base64 file data).
const MaxProtocolBytes = 90 << 20
