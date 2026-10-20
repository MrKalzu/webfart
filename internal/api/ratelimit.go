package api

import (
	"sync"
	"time"
	"webfart/internal/identity"
)

// RateLimiter enforces 1 post per 10 seconds per IP (/64 IPv6), and tracks global post rate for PoW.
type RateLimiter struct {
	mu           sync.Mutex
	lastPostByIP map[string]int64
	interval     int64 // minimum seconds between posts per IP

	// Global post tracking for adaptive PoW
	globalPosts  []int64
	powThreshold int // posts per 5 seconds to trigger PoW
	forcePoW     bool
}

// NewRateLimiter creates a RateLimiter with configurable post interval and PoW load threshold.
func NewRateLimiter(intervalSeconds, powThreshold int) *RateLimiter {
	if intervalSeconds <= 0 {
		intervalSeconds = 10 // default 10 seconds between posts
	}
	if powThreshold <= 0 {
		powThreshold = 10 // 10 posts in 5 seconds (~2 posts/sec) triggers Hashcash
	}
	rl := &RateLimiter{
		lastPostByIP: make(map[string]int64),
		interval:     int64(intervalSeconds),
		powThreshold: powThreshold,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now().Unix()
		for ip, last := range rl.lastPostByIP {
			if now-last > 300 {
				delete(rl.lastPostByIP, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// SetForcePoW allows forcing PoW requirement (useful for testing or emergency lockdown).
func (rl *RateLimiter) SetForcePoW(force bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.forcePoW = force
}

// CheckLimit checks if an IP is allowed to post. Returns retryAfter seconds (0 if allowed).
func (rl *RateLimiter) CheckLimit(clientIP string) (allowed bool, retryAfter int) {
	truncIP := identity.TruncateIP(clientIP)
	now := time.Now().Unix()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	last, exists := rl.lastPostByIP[truncIP]
	if exists {
		elapsed := now - last
		if elapsed < rl.interval {
			return false, int(rl.interval - elapsed)
		}
	}

	return true, 0
}

// RecordPost records a successful post for an IP and updates global load rate.
func (rl *RateLimiter) RecordPost(clientIP string) {
	truncIP := identity.TruncateIP(clientIP)
	now := time.Now().Unix()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.lastPostByIP[truncIP] = now
	rl.globalPosts = append(rl.globalPosts, now)

	// Prune old timestamps older than 5 seconds
	cutoff := now - 5
	idx := 0
	for idx < len(rl.globalPosts) && rl.globalPosts[idx] < cutoff {
		idx++
	}
	if idx > 0 {
		rl.globalPosts = rl.globalPosts[idx:]
	}
}

// IsPoWRequired returns true if the server is under high load or PoW is forced.
func (rl *RateLimiter) IsPoWRequired() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.forcePoW {
		return true
	}

	now := time.Now().Unix()
	cutoff := now - 5
	count := 0
	for _, ts := range rl.globalPosts {
		if ts >= cutoff {
			count++
		}
	}

	return count >= rl.powThreshold
}
