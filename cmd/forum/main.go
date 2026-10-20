package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"webfart/internal/api"
	"webfart/internal/pow"
	"webfart/internal/storage"
)

func loadOrGenerateKey(path string) (ed25519.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) == ed25519.PrivateKeySize {
			return ed25519.PrivateKey(data), nil
		}
		// Try hex decoding
		if dec, err := hex.DecodeString(string(data)); err == nil && len(dec) == ed25519.PrivateKeySize {
			return ed25519.PrivateKey(dec), nil
		}
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, priv, 0600); err != nil {
		log.Printf("Warning: failed persisting Ed25519 key to %s: %v", path, err)
	}
	return priv, nil
}

func loadOrGenerateSecret(path string, size int) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil && len(data) >= size {
		return data[:size], nil
	}

	secret := make([]byte, size)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, secret, 0600); err != nil {
		log.Printf("Warning: failed persisting secret to %s: %v", path, err)
	}
	return secret, nil
}

func main() {
	listenAddr := flag.String("listen", ":8080", "HTTP listen address")
	dataDir := flag.String("data", "./threads", "Directory for thread databases")
	staticDir := flag.String("static", "./web/static", "Directory for frontend static assets")
	sockPath := flag.String("sock", "/run/forum/admin.sock", "Path to admin unix domain socket")
	rateLimitSec := flag.Int("rate-limit", 10, "Minimum seconds between posts per IP")
	powThreshold := flag.Int("pow-threshold", 10, "Post rate in 5s to trigger Hashcash")
	forcePoW := flag.Bool("force-pow", false, "Force Hashcash proof-of-work on all posts")
	flag.Parse()

	log.Println("Starting Ephemeral Forum Server...")

	// 1. Secrets & Keys
	keyPath := filepath.Join(*dataDir, "server.key")
	privKey, err := loadOrGenerateKey(keyPath)
	if err != nil {
		log.Fatalf("Failed initializing Ed25519 signing key: %v", err)
	}
	pubKey := privKey.Public().(ed25519.PublicKey)
	log.Printf("Ed25519 Public Key: %x", pubKey)

	masterSecret, err := loadOrGenerateSecret(filepath.Join(*dataDir, "master.secret"), 32)
	if err != nil {
		log.Fatalf("Failed initializing master secret: %v", err)
	}

	tripKey, err := loadOrGenerateSecret(filepath.Join(*dataDir, "trip.secret"), 32)
	if err != nil {
		log.Fatalf("Failed initializing tripcode key: %v", err)
	}

	powKey, err := loadOrGenerateSecret(filepath.Join(*dataDir, "pow.secret"), 32)
	if err != nil {
		log.Fatalf("Failed initializing PoW key: %v", err)
	}

	// 2. Storage Manager
	mgr, err := storage.NewManager(*dataDir, privKey)
	if err != nil {
		log.Fatalf("Failed initializing Storage Manager: %v", err)
	}

	// 3. PoW Engine & Rate Limiter
	powEngine := pow.NewPoWEngine(powKey, 16)
	rl := api.NewRateLimiter(*rateLimitSec, *powThreshold)
	if *forcePoW {
		rl.SetForcePoW(true)
	}

	// 4. Admin Unix Socket Server
	adminSock := *sockPath
	adminSrv, err := api.NewAdminServer(adminSock, mgr)
	if err != nil {
		// Fallback to local socket in dataDir if /run/forum is not writable
		fallbackSock := filepath.Join(*dataDir, "admin.sock")
		log.Printf("Failed binding to %s (%v), falling back to %s", adminSock, err, fallbackSock)
		adminSrv, err = api.NewAdminServer(fallbackSock, mgr)
		if err != nil {
			log.Fatalf("Failed initializing admin socket: %v", err)
		}
	}

	// 5. HTTP API Server
	cfg := api.ServerConfig{
		MasterSecret: masterSecret,
		TripKey:      tripKey,
		PoWKey:       powKey,
		StaticDir:    *staticDir,
	}
	apiHandler := api.NewServer(cfg, mgr, powEngine, rl)

	httpServer := &http.Server{
		Addr:         *listenAddr,
		Handler:      apiHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// 6. Graceful shutdown handler
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Listening on HTTP %s (data: %s, static: %s)", *listenAddr, *dataDir, *staticDir)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-shutdownChan
	log.Println("Shutting down gracefully...")

	// 1. Terminate all active SSE streams so HTTP Shutdown does not block
	apiHandler.Close()

	// 2. Shut down HTTP listener and wait for pending short requests
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = httpServer.Shutdown(ctx)

	// 3. Close admin socket and storage engine (flushes WAL & open blocks)
	adminSrv.Close()
	mgr.Close()
	log.Println("Shutdown complete.")
}
