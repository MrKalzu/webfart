package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"strings"
	"time"
)

// TruncateIP normalizes an IP address. IPv4 is returned as-is; IPv6 is masked to /64.
func TruncateIP(ipStr string) string {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return ipStr
	}
	// Check if it's IPv4
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	// IPv6: mask to /64
	mask := net.CIDRMask(64, 128)
	masked := ip.Mask(mask)
	return masked.String()
}

// DeriveDailySalt derives the daily salt for a given UTC date:
// HMAC-SHA256(masterSecret, UTC_YYYY_MM_DD)
func DeriveDailySalt(masterSecret []byte, t time.Time) []byte {
	dateStr := t.UTC().Format("2006-01-02")
	mac := hmac.New(sha256.New, masterSecret)
	mac.Write([]byte(dateStr))
	return mac.Sum(nil)
}

// DerivePID derives the 8-character hex poster ID:
// HMAC-SHA256(daily_salt, ClientIP/64 || thread)[0:4] hex-encoded
func DerivePID(masterSecret []byte, clientIP, thread string, t time.Time) string {
	dailySalt := DeriveDailySalt(masterSecret, t)
	truncIP := TruncateIP(clientIP)
	mac := hmac.New(sha256.New, dailySalt)
	mac.Write([]byte(truncIP + thread))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

// ParseNick processes nickname and tripcode:
// If input contains '#', splits DisplayNick#SecretPhrase.
// trip = Base64URL(HMAC-SHA256(tripKey, SecretPhrase))[0:8]
// returns "DisplayNick!trip".
// If nick is empty, returns empty string (which will display as pid).
func ParseNick(rawNick string, tripKey []byte) string {
	rawNick = strings.TrimSpace(rawNick)
	if rawNick == "" {
		return ""
	}

	parts := strings.SplitN(rawNick, "#", 2)
	displayNick := parts[0]
	if len(parts) == 1 {
		return displayNick
	}

	secret := parts[1]
	mac := hmac.New(sha256.New, tripKey)
	mac.Write([]byte(secret))
	sum := mac.Sum(nil)
	trip := base64.RawURLEncoding.EncodeToString(sum)
	if len(trip) > 8 {
		trip = trip[:8]
	}

	if displayNick == "" {
		return "!" + trip
	}
	return displayNick + "!" + trip
}
