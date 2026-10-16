package skillfs

// Entry describes one direct child without following links.
type Entry struct {
	Name string
	Kind string
}

// File contains bounded bytes and an identity for stability checks.
type File struct {
	Data     []byte
	Identity string
}

// Request is the versioned discovery helper request. All paths are relative.
type Request struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Limit     int64  `json:"limit"`
}

// Response is a bounded helper reply. Error never includes file contents.
type Response struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries,omitempty"`
	Partial bool    `json:"partial,omitempty"`
	File    File    `json:"file,omitzero"`
	Error   string  `json:"error,omitempty"`
}
