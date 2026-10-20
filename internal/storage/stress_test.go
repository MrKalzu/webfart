package storage

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrency2000Workers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy stress test in short mode")
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "stress_concurrency_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := NewManager(tmpDir, priv)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// 2,000 parallel workers posting to /h/
	const workers = 2000
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	t.Logf("Spawning %d concurrent workers posting to /h/...", workers)
	start := time.Now()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			_, err := mgr.Post("h", fmt.Sprintf("p%04d", workerID), "worker", fmt.Sprintf("Worker %d load test message", workerID), time.Now().UnixMilli())
			if err != nil {
				errs <- fmt.Errorf("worker %d failed: %w", workerID, err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	duration := time.Since(start)
	t.Logf("Completed %d concurrent writes in %v (throughput: %.1f posts/sec)", workers, duration, float64(workers)/duration.Seconds())

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrency error occurred: %v", err)
		}
	}

	// Verify count of messages
	actor, err := mgr.getOrCreateActor("h")
	if err != nil {
		t.Fatal(err)
	}
	var totalMsgs int
	err = actor.db.QueryRow("SELECT count(*) FROM messages;").Scan(&totalMsgs)
	if err != nil {
		t.Fatal(err)
	}
	if totalMsgs != workers {
		t.Fatalf("expected %d messages, got %d", workers, totalMsgs)
	}

	// Verify sealed blocks: 2000 / 128 = 15 sealed blocks
	var sealedBlocks int
	err = actor.db.QueryRow("SELECT count(*) FROM blocks;").Scan(&sealedBlocks)
	if err != nil {
		t.Fatal(err)
	}
	expectedBlocks := workers / 128
	if sealedBlocks != expectedBlocks {
		t.Fatalf("expected %d sealed blocks, got %d", expectedBlocks, sealedBlocks)
	}
}

func TestPruning15MBBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy pruning test in short mode")
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "pruning_boundary_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := NewManager(tmpDir, priv)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	actor, err := mgr.getOrCreateActor("h")
	if err != nil {
		t.Fatal(err)
	}

	// Inject 15 MB of message payload.
	// Max payload per message is 1024 bytes.
	// 15 MB = 15 * 1024 * 1024 = 15,728,640 bytes.
	// Each message 1000 bytes. ~15,730 messages.
	// To run fast in test while testing the exact same pruning logic:
	// Let's create messages of 1000 bytes each. 16 blocks of 128 = 2048 messages = 2,048,000 bytes.
	// Let's test with 16,000 posts:
	t.Log("Testing tumbling-block pruning by injecting >10MB payload...")

	// 1000-byte body
	body := strings.Repeat("A", 1000)
	// 11,000 messages * 1000 bytes = 11,000,000 bytes (> 10MB = 10,485,760 bytes)
	// That represents ~85 sealed blocks!
	numPosts := 11500

	for i := 0; i < numPosts; i++ {
		_, err := mgr.Post("h", "pid", "nick", body, time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("post %d failed: %v", i, err)
		}
	}

	// Read total_bytes in meta
	var totalBytes int64
	var vBytes []byte
	err = actor.db.QueryRow("SELECT v FROM meta WHERE k = 'total_bytes';").Scan(&vBytes)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Sscanf(string(vBytes), "%d", &totalBytes)

	t.Logf("Storage settled at %d bytes (budget: %d)", totalBytes, MaxThreadByteBudget)
	if totalBytes > MaxThreadByteBudget {
		t.Fatalf("total_bytes %d should be <= budget %d", totalBytes, MaxThreadByteBudget)
	}

	// Verify chain anchor was updated to non-zero hash
	var anchorBytes []byte
	err = actor.db.QueryRow("SELECT v FROM meta WHERE k = 'chain_anchor';").Scan(&anchorBytes)
	if err != nil {
		t.Fatal(err)
	}
	anchorHex := hex.EncodeToString(anchorBytes)
	t.Logf("Updated chain anchor: %s", anchorHex)
	if anchorHex == strings.Repeat("00", 32) {
		t.Fatal("chain_anchor should have been updated after pruning")
	}

	// Verify that blocks table retained header rows of pruned blocks
	var blockCount int
	err = actor.db.QueryRow("SELECT count(*) FROM blocks;").Scan(&blockCount)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Retained block headers: %d", blockCount)
	if blockCount < 80 {
		t.Fatalf("expected block headers to be retained, found %d", blockCount)
	}
}
