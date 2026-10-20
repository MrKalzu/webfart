package pow

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PoWEngine manages challenge issuance and verification.
type PoWEngine struct {
	key        []byte
	difficulty int
	mu         sync.Mutex
	usedTokens map[string]int64
}

// NewPoWEngine creates a new Hashcash engine.
func NewPoWEngine(key []byte, difficulty int) *PoWEngine {
	if difficulty <= 0 {
		difficulty = 16
	}
	engine := &PoWEngine{
		key:        key,
		difficulty: difficulty,
		usedTokens: make(map[string]int64),
	}
	// Start cleanup goroutine for usedTokens
	go engine.cleanupLoop()
	return engine
}

func (e *PoWEngine) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		e.mu.Lock()
		now := time.Now().Unix()
		for token, expiry := range e.usedTokens {
			if now > expiry {
				delete(e.usedTokens, token)
			}
		}
		e.mu.Unlock()
	}
}

// Challenge represents the JSON challenge sent with HTTP 428.
type Challenge struct {
	Token      string `json:"token"`
	Difficulty int    `json:"difficulty"`
}

// GenerateChallenge creates a signed challenge token: "<epoch_ts>:<nonce>:<hmac>".
func (e *PoWEngine) GenerateChallenge() Challenge {
	epoch := time.Now().Unix()
	var nonceBytes [16]byte
	rand.Read(nonceBytes[:])
	nonceHex := hex.EncodeToString(nonceBytes[:])

	msg := fmt.Sprintf("%d:%s", epoch, nonceHex)
	mac := hmac.New(sha256.New, e.key)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))

	token := fmt.Sprintf("%s:%s", msg, sig)
	return Challenge{
		Token:      token,
		Difficulty: e.difficulty,
	}
}

// HasLeadingZeros checks if data has at least d leading zero bits.
func HasLeadingZeros(hash [32]byte, difficulty int) bool {
	fullBytes := difficulty / 8
	remBits := difficulty % 8

	for i := 0; i < fullBytes; i++ {
		if hash[i] != 0 {
			return false
		}
	}
	if remBits > 0 {
		mask := byte(0xFF << (8 - remBits))
		if (hash[fullBytes] & mask) != 0 {
			return false
		}
	}
	return true
}

// VerifySolution validates an incoming "X-PoW" header: "<token>:<counter>".
func (e *PoWEngine) VerifySolution(xpowHeader string) error {
	parts := strings.Split(xpowHeader, ":")
	if len(parts) != 4 {
		return errors.New("malformed X-PoW format, expected epoch:nonce:sig:counter")
	}

	epochStr := parts[0]
	nonceHex := parts[1]
	sigHex := parts[2]
	counterStr := parts[3]

	token := fmt.Sprintf("%s:%s:%s", epochStr, nonceHex, sigHex)

	// 1. Freshness check (<60 seconds)
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		return errors.New("invalid epoch timestamp")
	}
	now := time.Now().Unix()
	if now-epoch > 60 || epoch-now > 60 {
		return errors.New("challenge token expired")
	}

	// 2. Signature verification
	msg := fmt.Sprintf("%s:%s", epochStr, nonceHex)
	mac := hmac.New(sha256.New, e.key)
	mac.Write([]byte(msg))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sigHex), []byte(expectedSig)) {
		return errors.New("invalid token signature")
	}

	// 3. Replay prevention
	e.mu.Lock()
	if _, exists := e.usedTokens[token]; exists {
		e.mu.Unlock()
		return errors.New("challenge token already used")
	}
	e.mu.Unlock()

	// 4. PoW check: SHA256(token || counter)
	hasher := sha256.New()
	hasher.Write([]byte(token))
	hasher.Write([]byte(counterStr))
	var hash [32]byte
	copy(hash[:], hasher.Sum(nil))

	if !HasLeadingZeros(hash, e.difficulty) {
		return errors.New("insufficient work")
	}

	// Mark token as used with TTL 120s
	e.mu.Lock()
	e.usedTokens[token] = now + 120
	e.mu.Unlock()

	return nil
}

// Solve finds a counter satisfying the PoW challenge (useful for client/testing).
func Solve(token string, difficulty int) (string, error) {
	for i := uint64(0); ; i++ {
		counter := strconv.FormatUint(i, 10)
		hasher := sha256.New()
		hasher.Write([]byte(token))
		hasher.Write([]byte(counter))
		var hash [32]byte
		copy(hash[:], hasher.Sum(nil))

		if HasLeadingZeros(hash, difficulty) {
			return counter, nil
		}
	}
}
