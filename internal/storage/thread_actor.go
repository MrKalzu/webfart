package storage

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"webfart/internal/merkle"
)

const MaxThreadByteBudget = 10 * 1024 * 1024 // 10 MB

type postReq struct {
	pid       string
	nick      string
	body      string
	createdAt int64
	respChan  chan postResp
}

type postResp struct {
	msg Message
	err error
}

type tombstoneReq struct {
	seq      uint64
	respChan chan error
}

type sealReq struct {
	force    bool
	respChan chan error
}

// ThreadActor manages the single write loop for a thread's SQLite database.
type ThreadActor struct {
	thread     string
	dbPath     string
	signingKey ed25519.PrivateKey
	db         *sql.DB

	postChan      chan postReq
	tombstoneChan chan tombstoneReq
	sealChan      chan sealReq
	closeChan     chan chan struct{}

	subscribersMu sync.RWMutex
	subscribers   map[chan Message]struct{}

	lastPostTime time.Time
	closeOnce    sync.Once
}

// NewThreadActor creates and starts a ThreadActor for the given thread.
func NewThreadActor(thread, dbDir string, signingKey ed25519.PrivateKey) (*ThreadActor, error) {
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed creating db dir: %w", err)
	}

	dbPath := filepath.Join(dbDir, fmt.Sprintf("%s.db", thread))
	actor := &ThreadActor{
		thread:        thread,
		dbPath:        dbPath,
		signingKey:    signingKey,
		postChan:      make(chan postReq, 128),
		tombstoneChan: make(chan tombstoneReq, 16),
		sealChan:      make(chan sealReq, 8),
		closeChan:     make(chan chan struct{}),
		subscribers:   make(map[chan Message]struct{}),
	}

	return actor, nil
}

// Start launches the dedicated writer goroutine.
func (a *ThreadActor) Start() error {
	// Open write connection
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL", a.dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("failed opening thread db: %w", err)
	}
	db.SetMaxOpenConns(1) // Single writer connection
	if err := InitSchema(db); err != nil {
		db.Close()
		return fmt.Errorf("failed initializing schema: %w", err)
	}
	a.db = db

	go a.writerLoop()
	return nil
}

func (a *ThreadActor) writerLoop() {
	// Auto seal check timer every 1 minute
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case req := <-a.postChan:
			msg, err := a.handlePost(req.pid, req.nick, req.body, req.createdAt)
			req.respChan <- postResp{msg: msg, err: err}
			if err == nil {
				a.lastPostTime = time.UnixMilli(req.createdAt)
				a.broadcast(msg)
			}

		case req := <-a.tombstoneChan:
			err := a.handleTombstone(req.seq)
			req.respChan <- err

		case req := <-a.sealChan:
			err := a.handleSeal(req.force)
			req.respChan <- err

		case <-ticker.C:
			// Check 30-minute idle open block seal rule
			if !a.lastPostTime.IsZero() && time.Since(a.lastPostTime) >= 30*time.Minute {
				_ = a.handleSeal(false)
			}

		case doneChan := <-a.closeChan:
			// Flush seal if needed, then close DB
			_ = a.handleSeal(false)
			a.db.Close()
			close(doneChan)
			return
		}
	}
}

func (a *ThreadActor) handlePost(pid, nick, body string, createdAt int64) (Message, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()

	// 1. Determine next seq
	var maxSeq int64
	err = tx.QueryRow("SELECT coalesce(max(seq), 0) FROM messages;").Scan(&maxSeq)
	if err != nil {
		return Message{}, err
	}
	nextSeq := uint64(maxSeq + 1)

	// 2. Generate 16-byte random salt nonce
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Message{}, err
	}

	// 3. Compute leaf hash
	leaf := merkle.ComputeLeafHash(a.thread, nextSeq, pid, nick, createdAt, nonce, []byte(body))

	// 4. Insert message
	_, err = tx.Exec(`
		INSERT INTO messages (seq, pid, nick, body, nonce, created_at, leaf)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, nextSeq, pid, nick, []byte(body), nonce, createdAt, leaf[:])
	if err != nil {
		return Message{}, err
	}

	// 5. Update total_bytes in meta
	bodyLen := len([]byte(body))
	_, err = tx.Exec(`
		UPDATE meta SET v = CAST(v AS INTEGER) + ? WHERE k = 'total_bytes'
	`, bodyLen)
	if err != nil {
		return Message{}, err
	}

	// Commit message insert
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}

	msg := Message{
		Seq:       nextSeq,
		PID:       &pid,
		Nick:      &nick,
		Body:      &body,
		CreatedAt: createdAt,
	}

	// 6. Check if open block should be sealed (128 messages reached)
	a.checkAndSealCapacity()

	// 7. Check tumbling block pruning (10 MB budget)
	a.checkAndPrune()

	return msg, nil
}

func (a *ThreadActor) checkAndSealCapacity() {
	var lastSealedSeq uint64
	_ = a.db.QueryRow("SELECT coalesce(max(first_seq + count - 1), 0) FROM blocks;").Scan(&lastSealedSeq)

	var unsealedCount int
	_ = a.db.QueryRow("SELECT count(*) FROM messages WHERE seq > ?;", lastSealedSeq).Scan(&unsealedCount)

	if unsealedCount >= 128 {
		_ = a.sealBlock(128)
	}
}

func (a *ThreadActor) handleSeal(force bool) error {
	var lastSealedSeq uint64
	_ = a.db.QueryRow("SELECT coalesce(max(first_seq + count - 1), 0) FROM blocks;").Scan(&lastSealedSeq)

	var unsealedCount int
	_ = a.db.QueryRow("SELECT count(*) FROM messages WHERE seq > ?;", lastSealedSeq).Scan(&unsealedCount)

	if unsealedCount == 0 {
		return nil
	}
	if unsealedCount < 128 && !force && time.Since(a.lastPostTime) < 30*time.Minute {
		return nil
	}

	return a.sealBlock(unsealedCount)
}

func (a *ThreadActor) sealBlock(countToSeal int) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var lastIdx sql.NullInt64
	var lastSealedSeq uint64
	err = tx.QueryRow("SELECT max(idx), coalesce(max(first_seq + count - 1), 0) FROM blocks;").Scan(&lastIdx, &lastSealedSeq)
	if err != nil {
		return err
	}

	var newIdx uint64
	var prevHeaderHash [32]byte

	if !lastIdx.Valid {
		newIdx = 0
		var anchorBytes []byte
		_ = tx.QueryRow("SELECT v FROM meta WHERE k = 'chain_anchor';").Scan(&anchorBytes)
		if len(anchorBytes) == 32 {
			copy(prevHeaderHash[:], anchorBytes)
		}
	} else {
		newIdx = uint64(lastIdx.Int64 + 1)
		// Fetch previous block header details to compute its header hash
		var prevIdx, prevFirstSeq, prevCount uint64
		var prevRootBytes, prevPrevBytes []byte
		err = tx.QueryRow(`
			SELECT idx, first_seq, count, root, prev FROM blocks WHERE idx = ?;
		`, lastIdx.Int64).Scan(&prevIdx, &prevFirstSeq, &prevCount, &prevRootBytes, &prevPrevBytes)
		if err != nil {
			return err
		}

		var pRoot, pPrev [32]byte
		copy(pRoot[:], prevRootBytes)
		copy(pPrev[:], prevPrevBytes)

		prevHdr := &merkle.BlockHeader{
			Idx:            prevIdx,
			FirstSeq:       prevFirstSeq,
			Count:          prevCount,
			Root:           pRoot,
			PrevHeaderHash: pPrev,
		}
		prevHeaderHash = merkle.ComputeHeaderHash(prevHdr)
	}

	// Fetch leaves of messages to seal
	rows, err := tx.Query(`
		SELECT seq, leaf FROM messages WHERE seq > ? ORDER BY seq ASC LIMIT ?;
	`, lastSealedSeq, countToSeal)
	if err != nil {
		return err
	}
	defer rows.Close()

	var leaves [][32]byte
	var firstSeq uint64
	for rows.Next() {
		var s uint64
		var lBytes []byte
		if err := rows.Scan(&s, &lBytes); err != nil {
			return err
		}
		if firstSeq == 0 {
			firstSeq = s
		}
		var leaf [32]byte
		copy(leaf[:], lBytes)
		leaves = append(leaves, leaf)
	}
	if len(leaves) == 0 {
		return nil
	}

	root := merkle.ComputeRoot(leaves)
	header := &merkle.BlockHeader{
		Idx:            newIdx,
		FirstSeq:       firstSeq,
		Count:          uint64(len(leaves)),
		Root:           root,
		PrevHeaderHash: prevHeaderHash,
	}

	if err := merkle.SignHeader(header, a.signingKey); err != nil {
		return err
	}

	_, err = tx.Exec(`
		INSERT INTO blocks (idx, first_seq, count, root, prev, sig)
		VALUES (?, ?, ?, ?, ?, ?)
	`, header.Idx, header.FirstSeq, header.Count, header.Root[:], header.PrevHeaderHash[:], header.Sig)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (a *ThreadActor) checkAndPrune() {
	for {
		var totalBytes int64
		var vBytes []byte
		err := a.db.QueryRow("SELECT v FROM meta WHERE k = 'total_bytes';").Scan(&vBytes)
		if err != nil {
			return
		}
		totalBytes, _ = strconv.ParseInt(string(vBytes), 10, 64)
		if totalBytes <= MaxThreadByteBudget {
			return
		}

		// Find oldest sealed block with unpruned messages
		var bIdx, bFirstSeq, bCount uint64
		var bRootBytes, bPrevBytes, bSigBytes []byte
		err = a.db.QueryRow(`
			SELECT b.idx, b.first_seq, b.count, b.root, b.prev, b.sig
			FROM blocks b
			WHERE EXISTS (
				SELECT 1 FROM messages m
				WHERE m.seq >= b.first_seq AND m.seq < b.first_seq + b.count
			)
			ORDER BY b.idx ASC LIMIT 1;
		`).Scan(&bIdx, &bFirstSeq, &bCount, &bRootBytes, &bPrevBytes, &bSigBytes)
		if err != nil {
			// No more sealed blocks to prune (open block invariant preserved)
			return
		}

		tx, err := a.db.Begin()
		if err != nil {
			return
		}

		var deletedBytes int64
		_ = tx.QueryRow(`
			SELECT coalesce(sum(length(body)), 0) FROM messages
			WHERE seq >= ? AND seq < ?;
		`, bFirstSeq, bFirstSeq+bCount).Scan(&deletedBytes)

		_, err = tx.Exec(`
			DELETE FROM messages WHERE seq >= ? AND seq < ?;
		`, bFirstSeq, bFirstSeq+bCount)
		if err != nil {
			tx.Rollback()
			return
		}

		// Compute header hash of pruned block
		var bRoot, bPrev [32]byte
		copy(bRoot[:], bRootBytes)
		copy(bPrev[:], bPrevBytes)
		bHdr := &merkle.BlockHeader{
			Idx:            bIdx,
			FirstSeq:       bFirstSeq,
			Count:          bCount,
			Root:           bRoot,
			PrevHeaderHash: bPrev,
		}
		anchor := merkle.ComputeHeaderHash(bHdr)

		_, err = tx.Exec("UPDATE meta SET v = ? WHERE k = 'chain_anchor';", anchor[:])
		if err != nil {
			tx.Rollback()
			return
		}

		_, err = tx.Exec("UPDATE meta SET v = CAST(v AS INTEGER) - ? WHERE k = 'total_bytes';", deletedBytes)
		if err != nil {
			tx.Rollback()
			return
		}

		if err := tx.Commit(); err != nil {
			return
		}
	}
}

func (a *ThreadActor) handleTombstone(seq uint64) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var bodyBytes []byte
	err = tx.QueryRow("SELECT body FROM messages WHERE seq = ?;", seq).Scan(&bodyBytes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("message seq %d not found", seq)
		}
		return err
	}

	bodyLen := len(bodyBytes)

	_, err = tx.Exec(`
		UPDATE messages
		SET pid = NULL, nick = NULL, body = NULL, nonce = NULL
		WHERE seq = ?;
	`, seq)
	if err != nil {
		return err
	}

	if bodyLen > 0 {
		_, _ = tx.Exec("UPDATE meta SET v = CAST(v AS INTEGER) - ? WHERE k = 'total_bytes';", bodyLen)
	}

	return tx.Commit()
}

func (a *ThreadActor) broadcast(msg Message) {
	a.subscribersMu.RLock()
	defer a.subscribersMu.RUnlock()

	for ch := range a.subscribers {
		select {
		case ch <- msg:
		default:
			// Non-blocking drop if consumer buffer is full
		}
	}
}

// Subscribe returns a channel receiving newly committed messages for this thread.
func (a *ThreadActor) Subscribe() (chan Message, func()) {
	ch := make(chan Message, 64)
	a.subscribersMu.Lock()
	a.subscribers[ch] = struct{}{}
	a.subscribersMu.Unlock()

	cancel := func() {
		a.subscribersMu.Lock()
		delete(a.subscribers, ch)
		a.subscribersMu.Unlock()
	}
	return ch, cancel
}

// Post sends a post request to the actor.
func (a *ThreadActor) Post(pid, nick, body string, createdAt int64) (Message, error) {
	respChan := make(chan postResp, 1)
	a.postChan <- postReq{
		pid:       pid,
		nick:      nick,
		body:      body,
		createdAt: createdAt,
		respChan:  respChan,
	}
	res := <-respChan
	return res.msg, res.err
}

// Tombstone marks a message as tombstoned.
func (a *ThreadActor) Tombstone(seq uint64) error {
	respChan := make(chan error, 1)
	a.tombstoneChan <- tombstoneReq{seq: seq, respChan: respChan}
	return <-respChan
}

// Seal triggers block sealing (forced if force is true).
func (a *ThreadActor) Seal(force bool) error {
	respChan := make(chan error, 1)
	a.sealChan <- sealReq{force: force, respChan: respChan}
	return <-respChan
}

// Close shuts down the actor idempotently.
func (a *ThreadActor) Close() {
	a.closeOnce.Do(func() {
		done := make(chan struct{})
		a.closeChan <- done
		<-done
	})
}

// GetAnchor returns the current chain anchor hex string from meta.
func (a *ThreadActor) GetAnchor() (string, error) {
	var anchorBytes []byte
	err := a.db.QueryRow("SELECT v FROM meta WHERE k = 'chain_anchor';").Scan(&anchorBytes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return hex.EncodeToString(make([]byte, 32)), nil
		}
		return "", err
	}
	if len(anchorBytes) == 0 {
		return hex.EncodeToString(make([]byte, 32)), nil
	}
	return hex.EncodeToString(anchorBytes), nil
}
