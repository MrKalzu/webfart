package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"webfart/internal/identity"
	"webfart/internal/pow"
	"webfart/internal/storage"
)

// ServerConfig holds runtime dependencies for the HTTP API server.
type ServerConfig struct {
	MasterSecret []byte
	TripKey      []byte
	PoWKey       []byte
	StaticDir    string
	TrustedProxy string
}

// Server handles all HTTP requests for the forum.
type Server struct {
	cfg        ServerConfig
	mgr        *storage.Manager
	rateLimit  *RateLimiter
	powEngine  *pow.PoWEngine
	sseManager *SSEManager
	mux        *http.ServeMux
}

// NewServer initializes the HTTP server.
func NewServer(cfg ServerConfig, mgr *storage.Manager, powEngine *pow.PoWEngine, rl *RateLimiter) *Server {
	s := &Server{
		cfg:        cfg,
		mgr:        mgr,
		rateLimit:  rl,
		powEngine:  powEngine,
		sseManager: NewSSEManager(10, 200),
		mux:        http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	// API routes
	s.mux.HandleFunc("/api/index", s.handleAPIIndex)
	s.mux.HandleFunc("/api/challenge", s.handleAPIChallenge)
	s.mux.HandleFunc("/api/pubkey", s.handleAPIPubKey)
	s.mux.HandleFunc("/api/", s.handleAPIRouter)

	// Static assets
	s.mux.HandleFunc("/static/", s.handleStatic)

	// Frontend page router
	s.mux.HandleFunc("/", s.handleFrontend)
}

// GetClientIP extracts client IP, respecting X-Forwarded-For if from trusted proxy.
func GetClientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		clientPart := strings.TrimSpace(parts[0])
		if net.ParseIP(clientPart) != nil {
			return clientPart
		}
	}

	return remoteHost
}

func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	idx, err := s.mgr.GetIndex()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(idx)
}

func (s *Server) handleAPIChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	challenge := s.powEngine.GenerateChallenge()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(challenge)
}

func (s *Server) handleAPIPubKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	pubHex := hex.EncodeToString(s.mgr.PublicKey())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"algorithm": "ed25519",
		"public_key": pubHex,
	})
}

func (s *Server) handleAPIRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")

	// 1. Check for terminal .txt request: e.g. "h.txt"
	if strings.HasSuffix(path, ".txt") {
		thread := strings.TrimSuffix(path, ".txt")
		if !storage.ValidateThread(thread) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		HandleTerminalText(w, r, thread, s.mgr)
		return
	}

	// 2. Check for stream: e.g. "h/stream"
	if strings.HasSuffix(path, "/stream") {
		thread := strings.TrimSuffix(path, "/stream")
		if !storage.ValidateThread(thread) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.sseManager.HandleSSE(w, r, thread, s.mgr)
		return
	}

	// 3. Thread endpoint: e.g. "h"
	thread := path
	if !storage.ValidateThread(thread) {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetThread(w, r, thread)
	case http.MethodPost:
		s.handlePostThread(w, r, thread)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request, thread string) {
	var sinceSeq uint64
	if qSince := r.URL.Query().Get("since_seq"); qSince != "" {
		if s, err := strconv.ParseUint(qSince, 10, 64); err == nil {
			sinceSeq = s
		}
	}

	limit := 50
	if qLimit := r.URL.Query().Get("limit"); qLimit != "" {
		if l, err := strconv.Atoi(qLimit); err == nil {
			limit = l
		}
	}

	resp, err := s.mgr.GetMessages(thread, sinceSeq, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePostThread(w http.ResponseWriter, r *http.Request, thread string) {
	clientIP := GetClientIP(r)

	// 1. Rate Limit check
	allowed, retryAfter := s.rateLimit.CheckLimit(clientIP)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	// 2. Adaptive PoW check
	if s.rateLimit.IsPoWRequired() {
		xpow := r.Header.Get("X-PoW")
		if xpow == "" {
			challenge := s.powEngine.GenerateChallenge()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusPreconditionRequired)
			_ = json.NewEncoder(w).Encode(challenge)
			return
		}
		if err := s.powEngine.VerifySolution(xpow); err != nil {
			challenge := s.powEngine.GenerateChallenge()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusPreconditionRequired)
			_ = json.NewEncoder(w).Encode(challenge)
			return
		}
	}

	// 3. Validation pipeline
	rawNick, sanitizedMsg, err := ValidateAndParsePost(r)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			http.Error(w, httpErr.Message, httpErr.StatusCode)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	// 4. Derive identities
	now := time.Now()
	pid := identity.DerivePID(s.cfg.MasterSecret, clientIP, thread, now)
	nick := identity.ParseNick(rawNick, s.cfg.TripKey)
	if nick == "" {
		nick = pid
	}

	// 5. Post to storage
	msg, err := s.mgr.Post(thread, pid, nick, sanitizedMsg, now.UnixMilli())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 6. Record rate limit
	s.rateLimit.RecordPost(clientIP)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(msg)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/static/")
	filePath := filepath.Join(s.cfg.StaticDir, relPath)

	// Prevent path traversal
	cleanPath := filepath.Clean(filePath)
	cleanStaticDir := filepath.Clean(s.cfg.StaticDir)
	if !strings.HasPrefix(cleanPath, cleanStaticDir) {
		http.NotFound(w, r)
		return
	}

	if strings.HasSuffix(relPath, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(relPath, ".js") {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}

	applySecurityHeaders(w)
	http.ServeFile(w, r, filePath)
}

func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	if p != "" && !storage.ValidateThread(p) {
		http.NotFound(w, r)
		return
	}

	indexPath := filepath.Join(s.cfg.StaticDir, "index.html")
	content, err := os.ReadFile(indexPath)
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}

	applySecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func applySecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none';")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
