package storage

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setupTestManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "forum_storage_test_*")
	if err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(tmpDir, priv)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatal(err)
	}

	cleanup := func() {
		mgr.Close()
		os.RemoveAll(tmpDir)
	}

	return mgr, cleanup
}

func TestPostAndGetMessages(t *testing.T) {
	mgr, cleanup := setupTestManager(t)
	defer cleanup()

	// 1. Inactive thread before first post returns empty list
	resp, err := mgr.GetMessages("h", 0, 50)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(resp.Messages) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(resp.Messages))
	}

	// 2. Post a message
	msg1, err := mgr.Post("h", "c3b91a0f", "alice", "Hello world!", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if msg1.Seq != 1 {
		t.Fatalf("expected seq 1, got %d", msg1.Seq)
	}

	// 3. Post a second message
	msg2, err := mgr.Post("h", "7fa11c20", "bob", "Second post", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if msg2.Seq != 2 {
		t.Fatalf("expected seq 2, got %d", msg2.Seq)
	}

	// 4. Read messages
	resp, err = mgr.GetMessages("h", 0, 50)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resp.Messages))
	}

	// 5. Test pagination since_seq
	respSince, err := mgr.GetMessages("h", 1, 50)
	if err != nil {
		t.Fatalf("GetMessages since failed: %v", err)
	}
	if len(respSince.Messages) != 1 || respSince.Messages[0].Seq != 2 {
		t.Fatalf("expected 1 message with seq 2")
	}

	// 6. Test Tombstone
	if err := mgr.Tombstone("h", 1); err != nil {
		t.Fatalf("Tombstone failed: %v", err)
	}

	respAfterTombstone, err := mgr.GetMessages("h", 0, 50)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if respAfterTombstone.Messages[0].Body != nil {
		t.Fatalf("expected nil body after tombstone, got %v", *respAfterTombstone.Messages[0].Body)
	}
	if respAfterTombstone.Messages[0].Nick != nil {
		t.Fatalf("expected nil nick after tombstone")
	}
}

func TestBlockSealingAt128(t *testing.T) {
	mgr, cleanup := setupTestManager(t)
	defer cleanup()

	// Post 128 messages to thread "a"
	now := time.Now().UnixMilli()
	for i := 0; i < 128; i++ {
		_, err := mgr.Post("a", "user1", "anon", fmt.Sprintf("post %d", i), now+int64(i))
		if err != nil {
			t.Fatalf("Post %d failed: %v", i, err)
		}
	}

	// Wait slightly for background capacity check if needed
	actor, err := mgr.getOrCreateActor("a")
	if err != nil {
		t.Fatal(err)
	}

	// Query blocks table directly
	var blockCount int
	err = actor.db.QueryRow("SELECT count(*) FROM blocks;").Scan(&blockCount)
	if err != nil {
		t.Fatal(err)
	}
	if blockCount != 1 {
		t.Fatalf("expected 1 sealed block after 128 posts, got %d", blockCount)
	}
}

func TestConcurrentPostsNoBusy(t *testing.T) {
	mgr, cleanup := setupTestManager(t)
	defer cleanup()

	// 50 concurrent goroutines posting to the same thread
	const numGoroutines = 50
	const postsPerGoroutine = 10
	var wg sync.WaitGroup

	errs := make(chan error, numGoroutines*postsPerGoroutine)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for p := 0; p < postsPerGoroutine; p++ {
				_, err := mgr.Post("h", fmt.Sprintf("p%d", gid), "user", fmt.Sprintf("goroutine %d post %d", gid, p), time.Now().UnixMilli())
				if err != nil {
					errs <- err
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent post returned error: %v", err)
		}
	}

	resp, err := mgr.GetMessages("h", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages) != 100 {
		t.Fatalf("expected 100 messages with limit 100, got %d", len(resp.Messages))
	}
}

func TestAdHocThreadLifecycle(t *testing.T) {
	mgr, cleanup := setupTestManager(t)
	defer cleanup()

	// 'x' is ad-hoc thread
	// 1. GET before post -> no db file exists
	dbFile := filepath.Join(mgr.dbDir, "x.db")
	if _, err := os.Stat(dbFile); !os.IsNotExist(err) {
		t.Fatal("x.db should not exist initially")
	}

	idx, err := mgr.GetIndex()
	if err != nil {
		t.Fatal(err)
	}
	for _, tName := range idx.ActiveThreads {
		if tName == "x" {
			t.Fatal("x should not be in active threads before posting")
		}
	}

	// 2. Post to 'x' -> instantiates db file
	_, err = mgr.Post("x", "pidx", "userx", "Adhoc thread post", time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dbFile); os.IsNotExist(err) {
		t.Fatal("x.db should exist after posting")
	}

	// 3. Index should now include 'x'
	idx, err = mgr.GetIndex()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tName := range idx.ActiveThreads {
		if tName == "x" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("x should be in active threads after posting")
	}
}
