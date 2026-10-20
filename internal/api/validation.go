package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PostPayload is the incoming JSON format for a post.
type PostPayload struct {
	Nick    string `json:"nick"`
	Message string `json:"message"`
}

var emptyLineCollapseRegex = regexp.MustCompile(`\n{3,}`)

// SanitizeMessage processes the raw message according to Section 5.3:
// 1. CRLF normalized to LF.
// 2. Strip NUL, bidi overrides/isolates (U+202A-U+202E, U+2066-U+2069), ASCII control chars except \n.
// 3. Trim leading/trailing whitespace.
// 4. Collapse runs of > 3 empty lines into a single blank line.
// 5. Require at least 4 non-whitespace code points.
// 6. Assert <= 1024 bytes.
func SanitizeMessage(raw string) (string, error) {
	// 1. Normalize CRLF to LF
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	// 2. Filter characters
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Strip NUL
		if r == 0x00 {
			continue
		}
		// Strip bidi overrides / isolates: U+202A–U+202E, U+2066–U+2069
		if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			continue
		}
		// Strip ASCII control characters except \n (0x0A)
		if (r < 0x20 && r != '\n') || r == 0x7F {
			continue
		}
		b.WriteRune(r)
	}
	s = b.String()

	// 3. Trim leading and trailing whitespace
	s = strings.TrimSpace(s)

	// 4. Collapse vertical whitespace runs exceeding 3 empty lines into a single blank line.
	// 3 empty lines between text is 4 newlines (\n\n\n\n). A single blank line is \n\n.
	// Any run of 4 or more newlines becomes \n\n.
	s = emptyLineCollapseRegex.ReplaceAllString(s, "\n\n")

	// 5. Minimum 4 non-whitespace Unicode code points
	nonWsCount := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			nonWsCount++
		}
	}
	if nonWsCount < 4 {
		return "", errors.New("message must contain at least 4 non-whitespace characters")
	}

	// 6. Byte boundary enforcement (<= 1024 bytes)
	if len([]byte(s)) > 1024 {
		return "", fmt.Errorf("message exceeds 1024 bytes (length: %d)", len([]byte(s)))
	}

	return s, nil
}

// SanitizeNick validates the nickname field:
// Max 32 ASCII characters.
func SanitizeNick(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > 32 {
		return "", errors.New("nickname exceeds 32 characters")
	}
	// Assert ASCII
	for i := 0; i < len(s); i++ {
		if s[i] > 127 || (s[i] < 32 && s[i] != '\t') {
			return "", errors.New("nickname must contain valid ASCII characters only")
		}
	}
	return s, nil
}

// ValidateAndParsePost processes the HTTP request body through the full validation chain:
// 1. Content-Type check (application/json)
// 2. Raw UTF-8 validation
// 3. JSON decode
// 4. Nick & message sanitization
func ValidateAndParsePost(r *http.Request) (string, string, error) {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return "", "", &HTTPError{StatusCode: http.StatusUnsupportedMediaType, Message: "Content-Type must be application/json"}
	}

	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 10*1024))
	if err != nil {
		return "", "", &HTTPError{StatusCode: http.StatusBadRequest, Message: "failed to read request body"}
	}

	if !utf8.Valid(rawBody) {
		return "", "", &HTTPError{StatusCode: http.StatusBadRequest, Message: "request body is not valid UTF-8"}
	}

	var payload PostPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return "", "", &HTTPError{StatusCode: http.StatusBadRequest, Message: "malformed JSON: " + err.Error()}
	}

	nick, err := SanitizeNick(payload.Nick)
	if err != nil {
		return "", "", &HTTPError{StatusCode: http.StatusBadRequest, Message: err.Error()}
	}

	msg, err := SanitizeMessage(payload.Message)
	if err != nil {
		return "", "", &HTTPError{StatusCode: http.StatusBadRequest, Message: err.Error()}
	}

	return nick, msg, nil
}

// HTTPError represents an error with an associated HTTP status code.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return e.Message
}
