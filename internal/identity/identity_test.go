package identity

import (
	"testing"
	"time"
)

func TestDerivePID(t *testing.T) {
	masterSecret := []byte("test-master-secret-12345")
	t1 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 19, 23, 59, 59, 0, time.UTC)
	tNextDay := time.Date(2026, 10, 20, 0, 0, 1, 0, time.UTC)

	ip4 := "192.168.1.100"
	pid1 := DerivePID(masterSecret, ip4, "h", t1)
	pid2 := DerivePID(masterSecret, ip4, "h", t2)
	if pid1 != pid2 {
		t.Fatalf("PID should be stable within the same UTC day, got %s and %s", pid1, pid2)
	}
	if len(pid1) != 8 {
		t.Fatalf("PID length should be 8 hex chars, got %d", len(pid1))
	}

	pidNext := DerivePID(masterSecret, ip4, "h", tNextDay)
	if pidNext == pid1 {
		t.Fatalf("PID should change across UTC days")
	}

	// Stable per thread? Different threads should have different PIDs
	pidOtherThread := DerivePID(masterSecret, ip4, "a", t1)
	if pidOtherThread == pid1 {
		t.Fatalf("PID should be different across different threads")
	}

	// IPv6 /64 truncation
	ip6a := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	ip6b := "2001:0db8:85a3:0000:1111:2222:3333:4444"
	pid6a := DerivePID(masterSecret, ip6a, "h", t1)
	pid6b := DerivePID(masterSecret, ip6b, "h", t1)
	if pid6a != pid6b {
		t.Fatalf("IPv6 addresses in the same /64 should produce identical PID: %s vs %s", pid6a, pid6b)
	}
}

func TestParseNickAndTripcode(t *testing.T) {
	tripKey := []byte("secret-trip-key-67890")

	// Plain nick
	nick := ParseNick("alice", tripKey)
	if nick != "alice" {
		t.Fatalf("expected 'alice', got %s", nick)
	}

	// Empty nick
	nickEmpty := ParseNick("   ", tripKey)
	if nickEmpty != "" {
		t.Fatalf("expected empty string, got %s", nickEmpty)
	}

	// Tripcode
	nickTrip := ParseNick("alice#mypassword", tripKey)
	if len(nickTrip) < 10 || nickTrip[:6] != "alice!" {
		t.Fatalf("expected 'alice!<trip>', got %s", nickTrip)
	}

	// Consistency
	nickTrip2 := ParseNick("alice#mypassword", tripKey)
	if nickTrip != nickTrip2 {
		t.Fatalf("tripcode should be deterministic: %s vs %s", nickTrip, nickTrip2)
	}

	// Secret difference
	nickTrip3 := ParseNick("alice#otherpassword", tripKey)
	if nickTrip == nickTrip3 {
		t.Fatalf("different secret should yield different tripcode")
	}
}
