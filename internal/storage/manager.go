package storage

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var validThreadRegex = regexp.MustCompile(`^[a-z]$`)

var DefaultHomeRowThreads = []string{"a", "s", "d", "f", "g", "h", "j", "k", "l"}

// Manager manages per-thread database actors and read queries.
type Manager struct {
	dbDir      string
	signingKey ed25519.PrivateKey
	pubKey     ed25519.PublicKey

	actorsMu sync.RWMutex
	actors   map[string]*ThreadActor

	cacheMu sync.RWMutex
	cache   map[string]*ThreadSummary
}

// NewManager initializes the storage manager.
func NewManager(dbDir string, signingKey ed25519.PrivateKey) (*Manager, error) {
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed creating db dir: %w", err)
	}

	pubKey := signingKey.Public().(ed25519.PublicKey)

	mgr := &Manager{
		dbDir:      dbDir,
		signingKey: signingKey,
		pubKey:     pubKey,
		actors:     make(map[string]*ThreadActor),
		cache:      make(map[string]*ThreadSummary),
	}

	// Warm up existing thread databases on disk
	for r := 'a'; r <= 'z'; r++ {
		t := string(r)
		dbPath := filepath.Join(dbDir, fmt.Sprintf("%s.db", t))
		if _, err := os.Stat(dbPath); err == nil {
			if _, err := mgr.getOrCreateActor(t); err != nil {
				return nil, fmt.Errorf("failed initializing existing thread %s: %w", t, err)
			}
			mgr.refreshThreadCache(t)
		}
	}

	return mgr, nil
}

// ValidateThread checks if thread name is strictly ^[a-z]$.
func ValidateThread(thread string) bool {
	return validThreadRegex.MatchString(thread)
}

// PublicKey returns the server's Ed25519 public key.
func (m *Manager) PublicKey() ed25519.PublicKey {
	return m.pubKey
}

func (m *Manager) getOrCreateActor(thread string) (*ThreadActor, error) {
	if !ValidateThread(thread) {
		return nil, errors.New("invalid thread identifier")
	}

	m.actorsMu.Lock()
	defer m.actorsMu.Unlock()

	if actor, exists := m.actors[thread]; exists {
		return actor, nil
	}

	actor, err := NewThreadActor(thread, m.dbDir, m.signingKey)
	if err != nil {
		return nil, err
	}
	if err := actor.Start(); err != nil {
		return nil, err
	}

	m.actors[thread] = actor
	return actor, nil
}

// Post submits a new message to the thread, creating the database if it is the first post.
func (m *Manager) Post(thread, pid, nick, body string, createdAt int64) (Message, error) {
	if !ValidateThread(thread) {
		return Message{}, errors.New("invalid thread identifier")
	}

	actor, err := m.getOrCreateActor(thread)
	if err != nil {
		return Message{}, err
	}

	msg, err := actor.Post(pid, nick, body, createdAt)
	if err != nil {
		return Message{}, err
	}

	m.refreshThreadCache(thread)
	return msg, nil
}

// Tombstone removes message content for (thread, seq).
func (m *Manager) Tombstone(thread string, seq uint64) error {
	if !ValidateThread(thread) {
		return errors.New("invalid thread identifier")
	}

	dbPath := filepath.Join(m.dbDir, fmt.Sprintf("%s.db", thread))
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return fmt.Errorf("thread %s does not exist", thread)
	}

	actor, err := m.getOrCreateActor(thread)
	if err != nil {
		return err
	}

	if err := actor.Tombstone(seq); err != nil {
		return err
	}

	m.refreshThreadCache(thread)
	return nil
}

// Subscribe returns an SSE subscription channel for a thread.
func (m *Manager) Subscribe(thread string) (chan Message, func(), error) {
	if !ValidateThread(thread) {
		return nil, nil, errors.New("invalid thread identifier")
	}

	actor, err := m.getOrCreateActor(thread)
	if err != nil {
		return nil, nil, err
	}

	ch, cancel := actor.Subscribe()
	return ch, cancel, nil
}

// GetMessages reads paginated messages from the thread's database.
func (m *Manager) GetMessages(thread string, sinceSeq uint64, limit int) (ThreadResponse, error) {
	if !ValidateThread(thread) {
		return ThreadResponse{}, errors.New("invalid thread identifier")
	}

	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	dbPath := filepath.Join(m.dbDir, fmt.Sprintf("%s.db", thread))
	defaultAnchor := hex.EncodeToString(make([]byte, 32))

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return ThreadResponse{
			Thread:   thread,
			Anchor:   defaultAnchor,
			Messages: []Message{},
		}, nil
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return ThreadResponse{}, err
	}
	defer db.Close()

	// 1. Fetch chain anchor
	anchor := defaultAnchor
	var anchorBytes []byte
	_ = db.QueryRow("SELECT v FROM meta WHERE k = 'chain_anchor';").Scan(&anchorBytes)
	if len(anchorBytes) == 32 {
		anchor = hex.EncodeToString(anchorBytes)
	}

	// 2. Fetch messages > sinceSeq
	rows, err := db.Query(`
		SELECT seq, pid, nick, body, created_at
		FROM messages
		WHERE seq > ?
		ORDER BY seq ASC
		LIMIT ?;
	`, sinceSeq, limit)
	if err != nil {
		return ThreadResponse{}, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var seq uint64
		var pid, nick, body sql.NullString
		var createdAt int64

		if err := rows.Scan(&seq, &pid, &nick, &body, &createdAt); err != nil {
			return ThreadResponse{}, err
		}

		var pPID, pNick, pBody *string
		if pid.Valid {
			pPID = &pid.String
		}
		if nick.Valid {
			pNick = &nick.String
		}
		if body.Valid {
			pBody = &body.String
		}

		msgs = append(msgs, Message{
			Seq:       seq,
			PID:       pPID,
			Nick:      pNick,
			Body:      pBody,
			CreatedAt: createdAt,
		})
	}

	if msgs == nil {
		msgs = []Message{}
	}

	return ThreadResponse{
		Thread:   thread,
		Anchor:   anchor,
		Messages: msgs,
	}, nil
}

// GetIndex returns the home row threads plus active ad-hoc threads with posts in the trailing 24 hours.
func (m *Manager) GetIndex() (IndexResponse, error) {
	nowMs := time.Now().UnixMilli()
	cutoff24h := nowMs - 24*60*60*1000

	activeSet := make(map[string]bool)
	for _, t := range DefaultHomeRowThreads {
		activeSet[t] = true
	}

	// Check all other letters for posts in trailing 24h
	for r := 'a'; r <= 'z'; r++ {
		t := string(r)
		if activeSet[t] {
			continue
		}
		dbPath := filepath.Join(m.dbDir, fmt.Sprintf("%s.db", t))
		if _, err := os.Stat(dbPath); err == nil {
			// Query if there is a message created_at >= cutoff24h
			if m.hasRecentPost(t, cutoff24h) {
				activeSet[t] = true
			}
		}
	}

	var activeThreads []string
	for r := 'a'; r <= 'z'; r++ {
		t := string(r)
		if activeSet[t] {
			activeThreads = append(activeThreads, t)
		}
	}

	threadsMap := make(map[string]ThreadSummary)
	for _, t := range activeThreads {
		summary := m.getThreadSummary(t)
		threadsMap[t] = summary
	}

	return IndexResponse{
		ActiveThreads: activeThreads,
		Threads:       threadsMap,
	}, nil
}

func (m *Manager) hasRecentPost(thread string, cutoffMs int64) bool {
	dbPath := filepath.Join(m.dbDir, fmt.Sprintf("%s.db", thread))
	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return false
	}
	defer db.Close()

	var exists int
	_ = db.QueryRow("SELECT 1 FROM messages WHERE created_at >= ? LIMIT 1;", cutoffMs).Scan(&exists)
	return exists == 1
}

func (m *Manager) getThreadSummary(thread string) ThreadSummary {
	m.cacheMu.RLock()
	cached, exists := m.cache[thread]
	m.cacheMu.RUnlock()
	if exists && cached != nil {
		return *cached
	}

	m.refreshThreadCache(thread)

	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	if c, ok := m.cache[thread]; ok && c != nil {
		return *c
	}

	return ThreadSummary{
		HeadSeq: 0,
		Latest:  []Message{},
	}
}

func (m *Manager) refreshThreadCache(thread string) {
	dbPath := filepath.Join(m.dbDir, fmt.Sprintf("%s.db", thread))
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		m.cacheMu.Lock()
		m.cache[thread] = &ThreadSummary{HeadSeq: 0, Latest: []Message{}}
		m.cacheMu.Unlock()
		return
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return
	}
	defer db.Close()

	var headSeq uint64
	_ = db.QueryRow("SELECT coalesce(max(seq), 0) FROM messages;").Scan(&headSeq)

	rows, err := db.Query(`
		SELECT seq, pid, nick, body, created_at
		FROM messages
		ORDER BY seq DESC
		LIMIT 5;
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	var reversed []Message
	for rows.Next() {
		var seq uint64
		var pid, nick, body sql.NullString
		var createdAt int64

		if err := rows.Scan(&seq, &pid, &nick, &body, &createdAt); err != nil {
			return
		}

		var pPID, pNick, pBody *string
		if pid.Valid {
			pPID = &pid.String
		}
		if nick.Valid {
			pNick = &nick.String
		}
		if body.Valid {
			pBody = &body.String
		}

		reversed = append(reversed, Message{
			Seq:       seq,
			PID:       pPID,
			Nick:      pNick,
			Body:      pBody,
			CreatedAt: createdAt,
		})
	}

	// Reverse to ascending chronological order
	var latest []Message
	for i := len(reversed) - 1; i >= 0; i-- {
		latest = append(latest, reversed[i])
	}
	if latest == nil {
		latest = []Message{}
	}

	m.cacheMu.Lock()
	m.cache[thread] = &ThreadSummary{
		HeadSeq: headSeq,
		Latest:  latest,
	}
	m.cacheMu.Unlock()
}

// Close shuts down all actors.
func (m *Manager) Close() {
	m.actorsMu.Lock()
	defer m.actorsMu.Unlock()

	for _, actor := range m.actors {
		actor.Close()
	}
}
