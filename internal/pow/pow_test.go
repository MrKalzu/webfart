package pow

import (
	"fmt"
	"testing"
	"time"
)

func TestPoWChallengeAndVerification(t *testing.T) {
	key := []byte("pow-secret-key-12345")
	diff := 10 // Use 10 bits for fast unit test
	engine := NewPoWEngine(key, diff)

	challenge := engine.GenerateChallenge()
	counter, err := Solve(challenge.Token, challenge.Difficulty)
	if err != nil {
		t.Fatal(err)
	}

	xpow := fmt.Sprintf("%s:%s", challenge.Token, counter)
	if err := engine.VerifySolution(xpow); err != nil {
		t.Fatalf("verification should succeed, got %v", err)
	}

	// Replay protection: second attempt with same token must fail
	if err := engine.VerifySolution(xpow); err == nil {
		t.Fatal("replay of token should fail")
	}

	// Invalid signature
	tamperedToken := challenge.Token[:len(challenge.Token)-2] + "00"
	tamperedXpow := fmt.Sprintf("%s:%s", tamperedToken, counter)
	if err := engine.VerifySolution(tamperedXpow); err == nil {
		t.Fatal("tampered signature should fail")
	}
}

func TestPoWExpiry(t *testing.T) {
	key := []byte("pow-secret-key-12345")
	engine := NewPoWEngine(key, 8)

	oldEpoch := time.Now().Unix() - 70
	oldToken := fmt.Sprintf("%d:1234:sig", oldEpoch)
	xpow := fmt.Sprintf("%s:counter", oldToken)

	if err := engine.VerifySolution(xpow); err == nil {
		t.Fatal("expired token should fail")
	}
}
