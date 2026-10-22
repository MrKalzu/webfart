# Webfart API Documentation

**Webfart** is an ephemeral, minimalist, append-only forum and text board server written in Go. It organizes discussions into single-letter thread databases (`a` through `z`), cryptographically commits messages into sealed Merkle blocks with Ed25519 signatures, supports Server-Sent Events (SSE) for live streaming, and features an adaptive Hashcash Proof-of-Work (PoW) engine for DDoS and spam deterrence.

---

## 1. General Conventions

### Base URL & Thread Naming
* All REST endpoints reside under the `/api/` path.
* Thread identifiers are strictly restricted to a single lowercase ASCII letter matching `^[a-z]$` (e.g., `/api/h`, `/api/a`).
* Any thread request with an invalid thread identifier will return `404 Not Found`.

### Global Security Headers
All HTTP responses include the following security headers:
```http
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none';
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
```

### Client IP & Proxy Forwarding
The server extracts client IP addresses for identity derivation, rate limiting, and SSE connection accounting.
* If a proxy sends `X-Forwarded-For`, the first IP in the comma-separated list is parsed and used.
* IPv6 addresses are truncated to their `/64` prefix subnet before calculating rate limits or identities.

---

## 2. Authentication, Identity & Abuse Prevention

### Poster ID (`pid`) & Nicknames
* **Anonymous Identity (`pid`)**: Webfart does not use traditional user accounts. A deterministic 8-character Poster ID (`pid`) is derived using HMAC over the master secret, the client's truncated IP address, the thread name, and the current day/epoch.
* **Nicknames (`nick`)**: Optional. Maximum 32 ASCII characters. If left empty, it defaults to the user's generated `pid`. Tripcodes (e.g., `Name#secret`) are parsed using `trip.secret` into `Name!tripcode`.

### Rate Limiting
* Default per-IP rate limit: **1 post per 10 seconds** per IP (or `/64` IPv6 subnet).
* Exceeding the rate limit returns `429 Too Many Requests` with a `Retry-After: <seconds>` header.

### Adaptive Proof-of-Work (Hashcash)
When global incoming post volume exceeds the trigger threshold (default: **10 posts in 5 seconds**) or when forced by server configuration (`-force-pow`):
1. Any `POST` request without a valid `X-PoW` header is rejected with **`428 Precondition Required`** and a challenge payload.
2. The client must solve the challenge by finding a string counter such that:
   $$\text{SHA-256}(\text{token} \mathbin{\Vert} \text{counter})$$
   has at least `difficulty` leading zero bits (default: `16` bits / two full zero bytes).
3. The client retries the request including the header:
   ```http
   X-PoW: <epoch>:<nonce>:<hmac>:<counter>
   ```
4. Tokens expire after **60 seconds** and cannot be replayed (tracked with a 120-second TTL cache).

---

## 3. Data Models

### Message Object
Represents a single post within a thread.

| Field | Type | Description |
| :--- | :--- | :--- |
| `seq` | `uint64` | Monotonically increasing 1-based sequence number within the thread. |
| `pid` | `string \| null` | Ephemeral poster ID. Returns `null` if the message was tombstoned. |
| `nick` | `string \| null` | Nickname or `nick!trip`. Returns `null` if the message was tombstoned. |
| `body` | `string \| null` | Message text content. Returns `null` if the message was tombstoned. |
| `created_at` | `int64` | Timestamp of creation in Unix milliseconds. |

#### Message JSON Example
```json
{
  "seq": 42,
  "pid": "d3b07384",
  "nick": "anon!3g4kL0",
  "body": "Hello world from the terminal!",
  "created_at": 1718000000000
}
```

#### Tombstoned Message JSON Example
```json
{
  "seq": 42,
  "pid": null,
  "nick": null,
  "body": null,
  "created_at": 1718000000000
}
```

---

## 4. REST API Endpoints

### 4.1 Index & Global State

---

#### `GET /api/index`
Returns the list of active threads and a preview of the latest 5 messages per thread.

* **Active Threads Rule**: Always includes the "home row" threads (`a`, `s`, `d`, `f`, `g`, `h`, `j`, `k`, `l`) plus any ad-hoc threads (`b`, `c`, `e`, etc.) that have received a post within the trailing 24 hours.

##### Request
```http
GET /api/index HTTP/1.1
Host: example.com
```

##### Response (`200 OK`)
```json
{
  "active_threads": ["a", "s", "d", "f", "g", "h", "j", "k", "l", "z"],
  "threads": {
    "h": {
      "head_seq": 104,
      "latest": [
        {
          "seq": 100,
          "pid": "e9b21a0f",
          "nick": "alice",
          "body": "first post in window",
          "created_at": 1718000010000
        },
        {
          "seq": 101,
          "pid": "d3b07384",
          "nick": "bob",
          "body": "second post",
          "created_at": 1718000020000
        }
      ]
    }
  }
}
```

---

#### `GET /api/pubkey`
Returns the server’s Ed25519 public signing key in hex format. This key is used to verify Merkle block headers and historical integrity proofs.

##### Request
```http
GET /api/pubkey HTTP/1.1
Host: example.com
```

##### Response (`200 OK`)
```json
{
  "algorithm": "ed25519",
  "public_key": "4c6172736f6e...32_byte_hex_encoded_ed25519_public_key"
}
```

---

#### `GET /api/challenge`
Explicitly requests an active PoW Hashcash challenge token from the server without triggering an error.

##### Request
```http
GET /api/challenge HTTP/1.1
Host: example.com
```

##### Response (`200 OK`)
```json
{
  "token": "1718000000:7fa1e90b82c3d4f5:62f831...hmac_hex",
  "difficulty": 16
}
```

---

#### `POST /api/tripcode`
Derives the public display nick and callable tripcode from a `Nick#Secret` string without publishing a message. Secrets are processed exclusively in memory using the server's `trip.secret` and never persisted.

##### Request
```http
POST /api/tripcode HTTP/1.1
Host: example.com
Content-Type: application/json

{
  "nick": "alice#mysecretphrase"
}
```

##### Response (`200 OK`)
```json
{
  "nick": "alice!3g4kL0",
  "trip": "!3g4kL0"
}
```

---

### 4.2 Threads & Messaging

---

#### `GET /api/{thread}`
Retrieves a paginated list of messages from the specified single-letter thread.

##### Path Parameters
* `thread` *(string, required)*: Single lowercase letter (`^[a-z]$`).

##### Query Parameters
| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `since_seq` | `uint64` | `0` | Returns only messages with `seq > since_seq`. |
| `limit` | `int` | `50` | Maximum messages to return (min `1`, max `100`). |

##### Request
```http
GET /api/h?since_seq=100&limit=2 HTTP/1.1
Host: example.com
```

##### Response (`200 OK`)
```json
{
  "thread": "h",
  "anchor": "0000000000000000000000000000000000000000000000000000000000000000",
  "messages": [
    {
      "seq": 101,
      "pid": "a1b2c3d4",
      "nick": "anon",
      "body": "Hello there!",
      "created_at": 1718000100000
    },
    {
      "seq": 102,
      "pid": "e5f6a7b8",
      "nick": "charlie",
      "body": "General Kenobi!",
      "created_at": 1718000105000
    }
  ]
}
```

##### Status Codes
* `200 OK`: Messages successfully retrieved.
* `404 Not Found`: Invalid thread format (`!^[a-z]$`).
* `500 Internal Server Error`: Database query failed.

---

#### `POST /api/{thread}`
Submits a new post to the specified thread. Automatically provisions the thread database if it is the first post.

##### Path Parameters
* `thread` *(string, required)*: Target thread matching `^[a-z]$`.

##### Headers
* `Content-Type: application/json` *(required)*
* `X-PoW: <epoch>:<nonce>:<sig>:<counter>` *(conditional, required if PoW challenge is triggered or forced)*

##### Request Body (`application/json`)
| Field | Type | Required | Description / Sanitization Rules |
| :--- | :--- | :--- | :--- |
| `nick` | `string` | No | Nickname / tripcode. Max 32 printable ASCII characters. |
| `message` | `string` | **Yes** | Post content. Must satisfy sanitization rules below. |

###### Message Validation & Sanitization Pipeline:
1. **CRLF Normalization**: `\r\n` and `\r` are converted to `\n`.
2. **Character Filtering**:
   * Null bytes (`0x00`) are stripped.
   * Bidirectional overrides and isolates (`U+202A`–`U+202E`, `U+2066`–`U+2069`) are stripped.
   * ASCII control characters (`< 0x20` and `0x7F`) are stripped, except newline (`\n`).
3. **Whitespace Trimming**: Leading and trailing whitespace is stripped.
4. **Blank Line Collapsing**: Excessive vertical whitespace runs of 4 or more newlines (`\n{4,}`) are collapsed to a single blank line (`\n\n`).
5. **Length Checks**:
   * Must contain at least **2 non-whitespace Unicode code points**.
   * Total encoded size must not exceed **1024 bytes**.

##### Request Example
```http
POST /api/h HTTP/1.1
Host: example.com
Content-Type: application/json

{
  "nick": "alice",
  "message": "Hello, anonymous forum!"
}
```

##### Response Headers
* `X-Poster-Nick: <nick>` (e.g., `alice!3g4kL0` or poster `pid`)
* `X-Poster-Trip: <!trip>` (e.g., `!3g4kL0`, present only when a tripcode secret was supplied)

##### Response (`201 Created`)
```json
{
  "seq": 103,
  "pid": "d3b07384",
  "nick": "alice",
  "body": "Hello, anonymous forum!",
  "created_at": 1718000200000
}
```

##### Error Responses
* **`400 Bad Request`**: Malformed JSON, non-UTF-8 payload, nick >32 chars, non-ASCII nick, message < 2 non-whitespace chars, or message > 1024 bytes.
* **`404 Not Found`**: Invalid thread identifier.
* **`415 Unsupported Media Type`**: Missing or non-`application/json` `Content-Type`.
* **`428 Precondition Required`**: Adaptive PoW is active; returns challenge JSON (see PoW section).
* **`429 Too Many Requests`**: Client posted within the 10-second rate-limit window. Returns `Retry-After: <sec>`.

---

### 4.3 Streaming & Terminal Endpoints

---

#### `GET /api/{thread}/stream`
Subscribes to a real-time Server-Sent Events (SSE) stream for posts on a given thread.

##### Path Parameters
* `thread` *(string, required)*: Single lowercase letter (`^[a-z]$`).

##### Headers
* `Accept: text/event-stream`
* `Last-Event-ID: <seq>` *(optional)*: Replays backlogged messages after this sequence number before streaming live posts.

##### Query Parameters
* `since_seq` *(uint64, optional)*: Alternative to `Last-Event-ID`. Replays messages starting from `since_seq + 1` (up to 100 catchup messages).

##### Concurrency Limits
* Maximum **10 concurrent streams** per client IP.
* Maximum **200 concurrent streams** globally per thread.
* Exceeding either limit yields `429 Too Many Requests` (`"too many concurrent streams"`).

##### Stream Behavior
* **Catchup**: If `Last-Event-ID` or `since_seq` is specified, up to 100 historical messages are delivered first.
* **Events**: Each message emits an event named `message` with `id: <seq>`.
* **Heartbeat**: Every 15 seconds, a heartbeat ping (`: ping\n\n`) is sent to prevent proxy timeouts.

##### Stream Response Format
```http
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no

id: 104
event: message
data: {"seq":104,"pid":"a1b2c3d4","nick":"anon","body":"Real-time message incoming!","created_at":1718000300000}

: ping

id: 105
event: message
data: {"seq":105,"pid":"e5f6a7b8","nick":"bob","body":"Saw it instantly!","created_at":1718000315000}
```

---

#### `GET /api/{thread}.txt`
Terminal-friendly plaintext dump of messages formatted for CLI utilities (`curl`, `nc`).

##### Path Parameters
* `thread` *(string, required)*: Single lowercase letter (`^[a-z]$`).

##### Query Parameters
* `since_seq` *(uint64, optional, default: `0`)*: Messages with `seq > since_seq`.
* `limit` *(int, optional, default: `50`, max: `100`)*: Total messages to display.

##### Request Example
```bash
curl -s "https://example.com/api/h.txt?limit=2"
```

##### Response (`200 OK - text/plain; charset=utf-8`)
```text
[12:34:56] <d3b07384> alice (seq: 101)
  Hello world!
  Second line of post.

[12:35:10] <a1b2c3d4> (seq: 102)
  [message deleted]
```

---

## 5. Web Frontend & Static Assets

* `GET /`: Serves the single-page application `index.html`.
* `GET /{thread}`: If `{thread}` matches `^[a-z]$`, serves `index.html` to allow client-side SPA routing.
* `GET /static/{filepath}`: Serves static assets (CSS, JS). Path traversal containing `..` is blocked with `404 Not Found`.

---

## 6. Admin Control Socket (IPC)

Administrative maintenance is exposed over a local Unix Domain Socket (default: `/run/forum/admin.sock` with fallback to `<dataDir>/admin.sock`), created with file permissions `0600`.

### Protocol
The socket accepts single-line commands terminated by a newline (`\n`) in either **JSON** or **plain text** format, and replies with a JSON object.

### Actions

#### `tombstone`
Nullifies user data (`pid`, `nick`, `body`, `nonce`) for a specific message sequence while retaining the permanent cryptographic Merkle leaf hash.

##### Command Formats
* **JSON**:
  ```json
  {"action": "tombstone", "thread": "h", "seq": 102}
  ```
* **Plain Text**:
  ```text
  tombstone h 102
  ```

##### Response Examples
* Success:
  ```json
  {"ok": true}
  ```
* Failure:
  ```json
  {"ok": false, "error": "thread h does not exist"}
  ```

##### Socket Invocation Example (CLI)
```bash
echo '{"action":"tombstone","thread":"h","seq":102}' | nc -U /run/forum/admin.sock
```
