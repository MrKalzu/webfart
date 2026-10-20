package storage

// Message represents a forum message in JSON responses and internal structs.
type Message struct {
	Seq       uint64  `json:"seq"`
	PID       *string `json:"pid"`
	Nick      *string `json:"nick"`
	Body      *string `json:"body"`
	CreatedAt int64   `json:"created_at"`
}

// Block represents a sealed Merkle block record from the database.
type Block struct {
	Idx      uint64   `json:"idx"`
	FirstSeq uint64   `json:"first_seq"`
	Count    uint64   `json:"count"`
	Root     [32]byte `json:"root"`
	Prev     [32]byte `json:"prev"`
	Sig      []byte   `json:"sig"`
}

// ThreadSummary holds metadata for /api/index
type ThreadSummary struct {
	HeadSeq uint64    `json:"head_seq"`
	Latest  []Message `json:"latest"`
}

// IndexResponse represents the payload of GET /api/index
type IndexResponse struct {
	ActiveThreads []string                 `json:"active_threads"`
	Threads       map[string]ThreadSummary `json:"threads"`
}

// ThreadResponse represents the payload of GET /api/{thread}
type ThreadResponse struct {
	Thread   string    `json:"thread"`
	Anchor   string    `json:"anchor"`
	Messages []Message `json:"messages"`
}
