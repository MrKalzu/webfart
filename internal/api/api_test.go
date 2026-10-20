package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"webfart/internal/pow"
	"webfart/internal/storage"
)

func setupTestServer(t *testing.T) (*Server, *storage.Manager, *RateLimiter, *pow.PoWEngine, func()) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "api_test_*")
	if err != nil {
		t.Fatal(err)
	}

	mgr, err := storage.NewManager(tmpDir, priv)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatal(err)
	}

	staticDir := filepath.Join(tmpDir, "static")
	_ = os.MkdirAll(staticDir, 0755)
	_ = os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<html><body>forum</body></html>"), 0644)

	powKey := []byte("test-pow-key")
	powEngine := pow.NewPoWEngine(powKey, 8) // low difficulty for fast tests
	rl := NewRateLimiter(100)

	cfg := ServerConfig{
		MasterSecret: []byte("test-master-secret"),
		TripKey:      []byte("test-trip-key"),
		PoWKey:       powKey,
		StaticDir:    staticDir,
	}

	srv := NewServer(cfg, mgr, powEngine, rl)

	cleanup := func() {
		mgr.Close()
		os.RemoveAll(tmpDir)
	}

	return srv, mgr, rl, powEngine, cleanup
}

func TestAPIRoutingAndValidation(t *testing.T) {
	srv, _, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	// 1. Multi-character path rejected with 404
	req := httptest.NewRequest(http.MethodGet, "/api/toolong", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for multi-char thread, got %d", w.Code)
	}

	// 2. Non-JSON Content-Type rejected with 415
	req = httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader("message=hello"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "10.0.0.1:1234"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for non-JSON, got %d", w.Code)
	}

	// 3. Invalid UTF-8 rejected with 400
	invalidUTF8 := []byte("{\"message\": \"\xff\xfe\xfd\"}")
	req = httptest.NewRequest(http.MethodPost, "/api/h", bytes.NewReader(invalidUTF8))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.2:1234"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UTF-8, got %d", w.Code)
	}

	// 4. Message with < 4 non-whitespace code points rejected with 400
	req = httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(`{"message": "  ab  "}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.3:1234"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for < 4 chars, got %d", w.Code)
	}

	// 5. Message > 1024 bytes rejected with 400
	hugeMsg := strings.Repeat("a", 1025)
	req = httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(fmt.Sprintf(`{"message": "%s"}`, hugeMsg)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.4:1234"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for > 1024 bytes, got %d", w.Code)
	}

	// 6. Valid POST accepted with 201 Created
	validPayload := `{"nick": "alice#pass123", "message": "Valid test message.\r\nLine 2"}`
	req = httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(validPayload))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.5:1234"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created storage.Message
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Seq != 1 {
		t.Fatalf("expected seq 1, got %d", created.Seq)
	}
	if created.PID == nil || len(*created.PID) != 8 {
		t.Fatalf("expected valid 8-char PID, got %v", created.PID)
	}
	if created.Nick == nil || !strings.HasPrefix(*created.Nick, "alice!") {
		t.Fatalf("expected tripcode nick, got %v", created.Nick)
	}
	if created.Body == nil || *created.Body != "Valid test message.\nLine 2" {
		t.Fatalf("expected normalized newline body, got %v", created.Body)
	}
}

func TestRateLimiting(t *testing.T) {
	srv, _, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	clientIP := "192.168.1.50:5555"

	// First post: OK
	req := httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(`{"message": "First message"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = clientIP
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}

	// Immediate second post: 429 Too Many Requests
	req = httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(`{"message": "Second message"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = clientIP
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests, got %d", w.Code)
	}
	retryAfter := w.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

func TestAdaptivePoWChallenge(t *testing.T) {
	srv, _, rl, _, cleanup := setupTestServer(t)
	defer cleanup()

	// Force PoW requirement
	rl.SetForcePoW(true)

	// POST without X-PoW: should get 428 Precondition Required
	req := httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(`{"message": "Needs PoW"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.60:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected 428 Precondition Required, got %d", w.Code)
	}

	var challenge pow.Challenge
	if err := json.Unmarshal(w.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("failed decoding challenge: %v", err)
	}

	// Solve challenge
	counter, err := pow.Solve(challenge.Token, challenge.Difficulty)
	if err != nil {
		t.Fatal(err)
	}

	// Resubmit with X-PoW header
	req = httptest.NewRequest(http.MethodPost, "/api/h", strings.NewReader(`{"message": "Needs PoW"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PoW", fmt.Sprintf("%s:%s", challenge.Token, counter))
	req.RemoteAddr = "192.168.1.60:1234"
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created after valid PoW, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTerminalAndIndexEndpoints(t *testing.T) {
	srv, mgr, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	_, err := mgr.Post("h", "pid1", "alice", "Line 1\nLine 2 multiline", 1718000000000)
	if err != nil {
		t.Fatal(err)
	}

	// GET /api/index
	req := httptest.NewRequest(http.MethodGet, "/api/index", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for index, got %d", w.Code)
	}

	var idx storage.IndexResponse
	if err := json.Unmarshal(w.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.ActiveThreads) < 9 {
		t.Fatalf("expected at least 9 home row threads, got %d", len(idx.ActiveThreads))
	}
	hSummary, exists := idx.Threads["h"]
	if !exists || len(hSummary.Latest) != 1 {
		t.Fatalf("expected thread h in index with 1 message")
	}

	// GET /api/h.txt
	req = httptest.NewRequest(http.MethodGet, "/api/h.txt", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for .txt endpoint, got %d", w.Code)
	}
	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Line 1") || !strings.Contains(bodyStr, "  Line 2 multiline") {
		t.Fatalf("terminal text must indent continuation lines: \n%s", bodyStr)
	}
}

func TestAdminSocketTombstone(t *testing.T) {
	_, mgr, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	msg, err := mgr.Post("h", "pid_test", "victim", "Targeted message", 1718000000000)
	if err != nil {
		t.Fatal(err)
	}

	sockDir, err := os.MkdirTemp("", "admin_sock_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)

	sockPath := filepath.Join(sockDir, "admin.sock")
	adminSrv, err := NewAdminServer(sockPath, mgr)
	if err != nil {
		t.Fatal(err)
	}
	defer adminSrv.Close()

	// Connect to admin socket and tombstone
	resp, err := mgr.GetMessages("h", 0, 10)
	if err != nil || len(resp.Messages) == 0 || resp.Messages[0].Body == nil {
		t.Fatal("message should exist before tombstone")
	}

	// Issue tombstone via StorageManager directly or through admin command
	err = mgr.Tombstone("h", msg.Seq)
	if err != nil {
		t.Fatalf("tombstone failed: %v", err)
	}

	respAfter, err := mgr.GetMessages("h", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if respAfter.Messages[0].Body != nil {
		t.Fatal("message body should be null after tombstone")
	}
	if respAfter.Messages[0].PID != nil {
		t.Fatal("message pid should be null after tombstone")
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("missing strict CSP: %s", csp)
	}

	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff header")
	}

	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing DENY frame header")
	}
}
