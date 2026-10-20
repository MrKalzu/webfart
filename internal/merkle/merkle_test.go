package merkle

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
)

func TestMerkleLeafAndRoot(t *testing.T) {
	var leaves [][32]byte
	for i := 0; i < 128; i++ {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		leaf := ComputeLeafHash("h", uint64(i+1), "c3b91a0f", "alice", 1718000000000+int64(i), nonce, []byte(fmt.Sprintf("message %d", i)))
		leaves = append(leaves, leaf)
	}

	root := ComputeRoot(leaves)
	if root == [32]byte{} {
		t.Fatal("Merkle root should not be all zeros")
	}

	// Verify inclusion proof for each leaf in 128-leaf tree
	for i := 0; i < len(leaves); i++ {
		proof, err := ComputeInclusionProof(leaves, i)
		if err != nil {
			t.Fatalf("ComputeInclusionProof failed for leaf %d: %v", i, err)
		}
		if !VerifyInclusionProof(leaves[i], i, len(leaves), proof, root) {
			t.Fatalf("Inclusion proof failed for leaf %d", i)
		}
	}
}

func TestHeaderSerializationAndSign(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var root [32]byte
	var prev [32]byte
	rand.Read(root[:])
	rand.Read(prev[:])

	hdr := &BlockHeader{
		Idx:            0,
		FirstSeq:       1,
		Count:          128,
		Root:           root,
		PrevHeaderHash: prev,
	}

	serialized := SerializeHeader(hdr)
	if len(serialized) != 88 {
		t.Fatalf("expected 88 bytes, got %d", len(serialized))
	}

	deserialized, err := DeserializeHeader(serialized)
	if err != nil {
		t.Fatal(err)
	}
	if deserialized.Idx != hdr.Idx || deserialized.FirstSeq != hdr.FirstSeq || deserialized.Count != hdr.Count || deserialized.Root != hdr.Root || deserialized.PrevHeaderHash != hdr.PrevHeaderHash {
		t.Fatal("deserialized header does not match original")
	}

	if err := SignHeader(hdr, priv); err != nil {
		t.Fatal(err)
	}
	if len(hdr.Sig) != 64 {
		t.Fatalf("expected signature len 64, got %d", len(hdr.Sig))
	}

	if !VerifyHeader(hdr, pub) {
		t.Fatal("header signature verification failed")
	}

	// Tampering test
	hdr.Count = 127
	if VerifyHeader(hdr, pub) {
		t.Fatal("tampered header should fail signature verification")
	}
}
