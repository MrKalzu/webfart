package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"webfart/internal/storage"
)

func TestMerkleVerifyCLI(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "merkleverify_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := storage.NewManager(tmpDir, priv)
	if err != nil {
		t.Fatal(err)
	}

	// Post 128 messages to seal block 0
	for i := 0; i < 128; i++ {
		_, err := mgr.Post("h", "pid1", "alice", fmt.Sprintf("Message %d for merkle verification", i), time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}

	// Post another 128 messages to seal block 1
	for i := 128; i < 256; i++ {
		_, err := mgr.Post("h", "pid2", "bob", fmt.Sprintf("Message %d for merkle verification", i), time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}

	mgr.Close()

	dbPath := filepath.Join(tmpDir, "h.db")
	pubHex := hex.EncodeToString(pub)

	cmd := exec.Command("../../bin/merkleverify", "-db", dbPath, "-pubkey", pubHex)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("merkleverify failed: %v\nOutput: %s", err, string(out))
	}

	t.Logf("merkleverify output:\n%s", string(out))
}
