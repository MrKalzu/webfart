# Ephemeral Forum: Unified System Architecture & Project Plan

---

# PART I: Software Design & Architecture Specification

## 1. System Overview & Philosophy

The Ephemeral Forum is a high-throughput, minimalist, text-only discussion board. It blends the simplicity of early message boards with verifiable cryptographic auditability and modern single-binary Go engineering.

### Core Tenets
* **Zero Sessions & Ephemeral Storage:** No user accounts, passwords, or cookies. Storage runs on an active tumbling-block ring buffer capped at 10 MB per thread.
* **Append-Only Merkle Integrity:** Messages are sealed into 128-message Merkle blocks linked by a cryptographic hash chain, allowing replica verification and third-party verification without storing interior tree nodes.
* **Isolated File-Per-Thread Concurrency:** Each thread is an autonomous SQLite database managed by an isolated writer goroutine, eliminating write contention (`SQLITE_BUSY`).
* **Terminal & Browser Dual-Citizen:** First-class support for low-overhead terminal browsing (`curl`, plain text, SSE streams) alongside an uncompiled, dependency-free vanilla HTML5/CSS frontend (<10 KB).

---

## 2. Namespace & Thread Topology

### 2.1 Thread Identifiers
* Threads are strictly single ASCII characters: `^[a-z]$` (26 maximum possible threads).
* Thread paths are normalized to lowercase. Any path traversal attempt or multi-character path is rejected immediately at routing with `HTTP 404`.

### 2.2 Lifecycle & Visibility
* **Default Threads (Home Row):** `a`, `s`, `d`, `f`, `g`, `h`, `j`, `k`, `l` are pinned permanently to the index.
* **Ad-Hoc Threads:** The remaining 17 letters exist dynamically.
  * A `GET` request on an inactive ad-hoc thread returns an empty message list without creating database files.
  * An ad-hoc thread database is instantiated only upon the **first successful `POST`**.
  * Ad-hoc threads appear on the index page if they have had at least one post within the trailing 24-hour window.

---

## 3. Storage Architecture: Merkle-Backed SQLite Engine

Instead of a single monolithic database, every thread runs in its own dedicated database file: `threads/{thread}.db`.

```
threads/
├── a.db
├── h.db
├── ...
└── l.db
```

```
+-------------------------------------------------------------+
|                     HTTP / Transport Layer                  |
|          GET /api/h          POST /api/h         SSE        |
+-------------------------------------------------------------+
                               |
         +---------------------+---------------------+
         | (Read-Only Pool)                          | (Write Channel)
         v                                           v
+------------------+                    +---------------------------+
| SQLite Reader(s) |                    | Single Thread-Writer Loop |
| threads/h.db     |                    | (owns write tx & state)   |
+------------------+                    +---------------------------+
         |                                           |
         |                               +-----------+-----------+
         |                               |                       |
         v                               v                       v
+------------------+            +------------------+    +-------------------+
| Read Committed   |            | SQLite WAL Write |    | Merkle Block Seal |
| Messages / Meta  |            | messages, blocks |    | & Prune (10 MB)   |
+------------------+            +------------------+    +-------------------+
                                         |
                                         v
                                +-------------------+
                                | Event Broadcaster | ---> SSE Subscribers
                                +-------------------+
```

### 3.1 Database Schema
```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA max_page_count = 8192; -- Hard physical budget cap (~32 MB at 4KB page size)

-- Sequenced message entries
CREATE TABLE messages (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    pid        TEXT,             -- Poster ID; NULL once tombstoned
    nick       TEXT,             -- Display nick (or nick!trip); NULL once tombstoned
    body       BLOB,             -- Message text; NULL once tombstoned
    nonce      BLOB,             -- Per-message random salt; NULL once tombstoned
    created_at INTEGER NOT NULL, -- Unix millisecond timestamp
    leaf       BLOB NOT NULL     -- 32-byte cryptographic leaf hash (retained permanently)
);

-- Sealed block headers (128 messages per block)
CREATE TABLE blocks (
    idx       INTEGER PRIMARY KEY, -- seq >> 7
    first_seq INTEGER NOT NULL,
    count     INTEGER NOT NULL,
    root      BLOB NOT NULL,       -- Merkle root of the block's leaves
    prev      BLOB NOT NULL,       -- Hash of previous block header
    sig       BLOB NOT NULL        -- Server signature over current block header
);

-- Internal bookkeeping
CREATE TABLE meta (
    k TEXT PRIMARY KEY,
    v BLOB
);
-- Keys:
-- 'chain_anchor': 32-byte hash of the most recently pruned block header
-- 'total_bytes' : 64-bit integer tracking total raw message footprint
-- 'epoch'       : Key/server generation ID
```

### 3.2 Concurrency Architecture
* **Single-Writer Actor Pattern:** Each active thread initializes a dedicated Go channel (`chan writeRequest`) consumed by a single goroutine owning an exclusive SQLite read-write connection.
* **Lock-Free Reads:** Read queries (`GET /api/{t}`, `.txt`, index previews) open shared read-only connections (`file:threads/{t}.db?mode=ro&_journal_mode=WAL`).
* This eliminates `SQLITE_BUSY` errors entirely by design.

---

## 4. Cryptographic Block Pipeline & Pruning

```
Message 000  --- (Leaf 000) \
Message 001  --- (Leaf 001)  \
...                            +--> Merkle Tree ---> Block Root 0
Message 127  --- (Leaf 127)  /     (recomputed       + Header Hash (Block 0)
                            /       on the fly)             |
                                                            v
Message 128  --- (Leaf 128) \                        Prev Header Hash
Message 129  --- (Leaf 129)  \                              |
...                            +--> Merkle Tree ---> Block Root 1
Message 255  --- (Leaf 255)  /                       + Header Hash (Block 1)
                            /                               |
                                                            v
[ Open Block: 1-127 Msgs ]                         Prev Header Hash
(Unsealed: seals at 128 or timer)                           |
                                                            v
                                                     [ Active Head ]
```

### 4.1 Leaf Construction (RFC 6962-Style)
Interior nodes are never persisted to disk. They are recomputed in microseconds from stored leaves when servicing proof requests.
* **Leaf Hash:** Computed using a `0x00` domain-separation byte over a length-prefixed binary encoding of fields:
  $$\text{Leaf} = \text{SHA256}(0x00 \mathbin{\Vert} \text{len}(t) \mathbin{\Vert} t \mathbin{\Vert} \text{seq} \mathbin{\Vert} \text{pid} \mathbin{\Vert} \text{nick} \mathbin{\Vert} \text{created\_at} \mathbin{\Vert} \text{nonce} \mathbin{\Vert} \text{body})$$
* **Interior Nodes:** Formed using a `0x01` prefix:
  $$\text{Node} = \text{SHA256}(0x01 \mathbin{\Vert} \text{LeftChild} \mathbin{\Vert} \text{RightChild})$$

### 4.2 Block Sealing Rules
1. **Capacity Trigger:** Every 128 messages (`count == 128`, or `seq & 0x7F == 0`), the open block seals.
2. **Timer Trigger:** If a thread has an unsealed open block with $1 \le \text{count} < 128$ and no new posts arrive for 30 minutes, an automated timer seals the block, recording the partial count in the block header.
3. **Block Header Serialization:**
   $$\text{Header} = \text{idx} \mathbin{\Vert} \text{first\_seq} \mathbin{\Vert} \text{count} \mathbin{\Vert} \text{root} \mathbin{\Vert} \text{prev\_header\_hash}$$
4. **Checkpoint:** Server signs $\text{SHA256}(\text{Header})$ using its persistent Ed25519 signing key and stores it in the `blocks.sig` column.

### 4.3 Tumbling Block Pruning
* When `meta['total_bytes'] > 10,485,760` (10 MB):
  1. The oldest sealed block ($B_{old}$) is selected for eviction.
  2. The message records associated with $B_{old}$ are deleted:
     ```sql
     DELETE FROM messages WHERE seq >= B_old.first_seq AND seq < (B_old.first_seq + B_old.count);
     ```
  3. `meta['chain_anchor']` is updated to $\text{SHA256}(\text{Header}(B_{old}))$.
  4. The size of the deleted messages is deducted from `meta['total_bytes']`.
  5. The `blocks` header row is retained (~100 bytes), preserving the hash chain structure.
  6. **Invariant:** The active *open block* is never pruned.

---

## 5. Identity, Tripcodes, & Defensive Engineering

### 5.1 Deterministic Poster ID (`pid`)
To eliminate session tracking while providing visual conversational context:
* **Derivation Formula:**
  $$\text{daily\_salt} = \text{HMAC-SHA256}(\text{MasterSecret}, \text{UTC\_YYYY\_MM\_DD})$$
  $$\text{pid} = \text{HMAC-SHA256}(\text{daily\_salt}, \text{ClientIP}_{/64} \mathbin{\Vert} \text{thread})[0:8]$$
* **Properties:**
  * Changes every 24 hours at 00:00 UTC automatically.
  * IPv6 addresses are truncated to `/64` to prevent subnet spoofing and privacy leakage.
  * Stable across threads? No. A poster has a different `pid` in `/a/` than in `/h/`.
  * If the poster provides no nickname, `pid` is rendered as the primary screen identity.

### 5.2 Optional Tripcodes
* Users may input nicks formatted as: `DisplayNick#SecretPhrase`.
* The server splits at the first `#`:
  $$\text{trip} = \text{Base64URL}(\text{HMAC-SHA256}(K_{trip}, \text{SecretPhrase}))[0:8]$$
* Stored in the database as: `DisplayNick!trip`.
* The raw secret phrase is cleared from memory immediately and never persisted or leaked into the leaf hash.

### 5.3 Request Validation Pipeline
Incoming `POST` requests must run through a non-short-circuiting validation chain:
1. **Transport Check:** `Content-Type` must be strictly `application/json`; otherwise reject with `HTTP 415 Unsupported Media Type` (enforcing browser CORS preflight).
2. **UTF-8 Validation:** Validate raw request bytes prior to JSON decoding using Go's `utf8.Valid()`. Reject with `HTTP 400` if invalid.
3. **Control Character Sanitization:**
   * Strip NUL (`0x00`), Unicode bidirectional overrides/isolates (`U+202A`–`U+202E`, `U+2066`–`U+2069`), and all ASCII control characters except line feeds (`\n`).
   * Normalize CRLF (`\r\n`) to LF (`\n`).
4. **Content Rules:**
   * Trim leading and trailing whitespace.
   * Require a minimum of 4 non-whitespace Unicode code points.
   * Collapse vertical whitespace runs exceeding 3 empty lines into a single blank line.
5. **Byte Boundary Enforcement:**
   * Assert $\text{len}([\,] \text{byte}(\text{message})) \le 1024$ bytes.
   * Assert $\text{len}([\,] \text{byte}(\text{nick})) \le 32$ ASCII characters.

---

## 6. Anti-Spam: Rate Limiter & Adaptive Hashcash

```
Incoming Request
       |
       v
+-------------------------------+
| Rate Limit Check (IP /64)     | ---> <10s interval? ---> HTTP 429 (Retry-After)
+-------------------------------+
       |
       v
+-------------------------------+
| High Global Load Threshold?   |
+-------------------------------+
       |                 |
      NO                YES
       |                 |
       v                 v
[ Process POST ]   +---------------------------+
                   | Valid "X-PoW" attached?   | ---> NO  ---> HTTP 428 (Challenge)
                   +---------------------------+
                         | (YES)
                         v
                   [ Verify & Process POST ]
```

### 6.1 Standard Rate Limiting
* Keyed exclusively to client IP (IPv6 truncated to `/64`).
* Minimum 10 seconds between posts across all threads.
* Rapid hits return `HTTP 429 Too Many Requests` with a `Retry-After: 10` header.

### 6.2 Adaptive Proof-of-Work (Hashcash)
Under baseline conditions, PoW is inactive (`curl` works out of the box). When the global ingest rate crosses an operational threshold:
1. Unauthenticated posts receive `HTTP 428 Precondition Required` containing a JSON challenge:
   ```json
   {
     "token": "1718000000:d8a2f...:sig",
     "difficulty": 16
   }
   ```
2. Challenge generation:
   $$\text{token} = \text{epoch\_ts} \mathbin{\Vert} \text{random\_nonce} \mathbin{\Vert} \text{HMAC-SHA256}(K_{pow}, \text{epoch\_ts} \mathbin{\Vert} \text{random\_nonce})$$
3. Client computes counter satisfying:
   $$\text{SHA256}(\text{token} \mathbin{\Vert} \text{counter}) < 2^{(256 - d)}$$
4. Client resubmits with header: `X-PoW: <token>:<counter>`.
5. Server verifies HMAC signature, verifies counter freshness ($<60$ seconds), and checks that the token is not present in the single-use cache.

---

## 7. Complete API Specification

### 7.1 Web API Endpoints

#### `GET /api/index`
Returns default home-row threads plus any ad-hoc threads with posts in the trailing 24 hours, including their 5 latest messages.

* **Response (`200 OK`):**
  ```json
  {
    "active_threads": ["a", "s", "d", "f", "g", "h", "j", "k", "l", "x"],
    "threads": {
      "h": {
        "head_seq": 8421,
        "latest": [
          {
            "seq": 8421,
            "pid": "c3b91a0f",
            "nick": "alice!9xK3la==",
            "body": "System architecture running cleanly.",
            "created_at": 1718000010450
          }
        ]
      }
    }
  }
  ```

#### `GET /api/{thread:[a-z]}`
Fetches paginated messages.

* **Query Parameters:**
  * `since_seq` (integer, optional): Fetch messages with `seq > since_seq`.
  * `limit` (integer, default: `50`, max: `100`).
* **Response (`200 OK`):**
  ```json
  {
    "thread": "h",
    "anchor": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "messages": [
      {
        "seq": 8420,
        "pid": "7fa11c20",
        "nick": "7fa11c20",
        "body": "Testing Merkle integrity.",
        "created_at": 1718000002100
      }
    ]
  }
  ```

#### `POST /api/{thread:[a-z]}`
Submits a message to the target thread.

* **Headers:**
  * `Content-Type: application/json` (Required)
  * `X-PoW: <token>:<counter>` (Conditional, during load sheds)
* **Payload:**
  ```json
  {
    "nick": "mod#mypassword",
    "message": "Thread notice: automated housekeeping."
  }
  ```
* **Response:**
  * `201 Created`: Returns newly appended message object (with calculated `seq`, `pid`, and `created_at`).
  * `400 Bad Request`: Validation failure.
  * `415 Unsupported Media Type`: Non-JSON content type.
  * `428 Precondition Required`: Hashcash challenge required.
  * `429 Too Many Requests`: Rate limit active.

---

### 7.2 Streaming & Terminal Endpoints

#### `GET /api/{thread:[a-z]}/stream`
Server-Sent Events (SSE) feed. Dispatches events as commits happen in SQLite.
* Supports standard `Last-Event-ID` header.
* Format:
  ```text
  id: 8422
  event: message
  data: {"seq":8422,"pid":"c3b91a0f","nick":"alice!9xK3la==","body":"Instant sync.","created_at":1718000020100}
  ```

#### `GET /api/{thread:[a-z]}.txt`
Formatted ASCII feed for plain terminal consumption (`curl -s https://host/api/h.txt`).
* Multi-line messages are formatted with 2-space indentation on continuation lines.
* Query parameters: `since_seq`, `limit`.

---

### 7.3 Admin Interprocess Protocol (Unix Domain Socket)
A local control socket runs at `/run/forum/admin.sock`.
* **Action:** Tombstone message `(thread, seq)`.
* **Execution:**
  1. CLI tool connects: `forumctl tombstone -t h -s 8420`.
  2. The message is routed to the thread's isolated writer channel.
  3. The writer zeroes out `pid`, `nick`, `body`, and `nonce`, while keeping the `seq`, `created_at`, and `leaf` hash.
  4. Root continuity is preserved; leaf verification remains valid.

---

## 8. Frontend & User Experience Specification

```
+---------------------------------------------------------------------------------+
| [a] [s] [d] [f] [g] [h*] [j] [k] [l]   [x]                        Thread: /h/   |
+---------------------------------------------------------------------------------+
|                                                                                 |
|  [21:40:12] <c3b91a0f> alice!9xK3la== (seq: 8421)                              |
|  +---------------------------------------------------------------------------+  |
|  | >>8420 Look at how deterministic tints reflect identity.                  |  |
|  +---------------------------------------------------------------------------+  |
|                                                                                 |
|  [21:41:05] <7fa11c20> (seq: 8422)                                           |
|  +---------------------------------------------------------------------------+  |
|  | Seamless event streams; zero page reloads.                                |  |
|  +---------------------------------------------------------------------------+  |
|                                                                                 |
+---------------------------------------------------------------------------------+
| [ Nick#Secret          ] [ Write your message here...                  ] [ Send ]|
+---------------------------------------------------------------------------------+
```

### 8.1 Styling & Viewport Rules
* **Theme:** Pure dark mode.
  * Base Background: `#0c0d0e`
  * Message Card: Base `#151618`, tinted dynamically with author's `pid`.
  * Text Color: `#d8dade`
  * Monospace Stack: `ui-monospace, "SF Mono", "Cascadia Code", monospace`
* **Pinned Input Anchor:** The bottom message bar is styled with `height: 100dvh` flex layout logic to avoid mobile virtual keyboard overlap bugs.
* **Quote Targeting:** Inline quotes `>>8420` render to hyperlinks anchor `#m8420`. The CSS selector `:target` smoothly highlights the target element:
  ```css
  .message:target {
    outline: 1px solid #4f87e2;
    background-color: #1e222b !important;
  }
  ```

### 8.2 Client Scripting Architecture (<10 KB, No Dependencies)
* **Nick URL Anchor:** Visiting `/h/#alice` fills the Nick field with `alice` without saving state or triggering network telemetry.
* **Smart Auto-Scroll:** New messages auto-scroll to the bottom **only** if the user is already within 50px of the viewport floor; otherwise, an unread message indicator button appears.
* **Lifecycle Stream Throttling:** Utilizes `document.addEventListener("visibilitychange", ...)`:
  * When tab is hidden: Closes EventSource stream.
  * When tab is visible: Reconnects EventSource stream passing `Last-Event-ID: <latest_seq>`.
* **Deterministic Tinting Implementation:**
  ```javascript
  function applyTint(element, pid) {
    let hash = 0;
    for (let i = 0; i < pid.length; i++) {
      hash = pid.charCodeAt(i) + ((hash << 5) - hash);
    }
    const hue = Math.abs(hash % 360);
    // Applied directly through style property to comply with CSP style-src 'self'
    element.style.backgroundColor = `hsl(${hue}, 20%, 13%)`;
  }
  ```

### 8.3 Content Security Policy (CSP)
Strict security headers shipped with every HTML document:
```http
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none';
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
```

---

# PART II: Project Delivery Plan

```
PHASE 1: Foundation (Days 1-4)
[SQLite Schema & Writer Engine]
            |
            v
PHASE 2: Merkle & Integrity (Days 5-7)
[128-Message Blocks, Pruning & Anchors]
            |
            v
PHASE 3: Transport & Defense (Days 8-10)
[JSON, SSE, Text APIs, PoW & Rate Limiting]
            |
            v
PHASE 4: Frontend UI (Days 11-12)
[Semantic HTML, Dark Theme, <10KB Vanilla JS]
            |
            v
PHASE 5: Operational Hardening (Days 13-14)
[Load Testing, Concurrency & Release Prep]
```

## Milestone 1: Core Engine & Per-Thread SQLite Pipeline
**Objective:** Deliver isolated thread database managers with serialized write routines.
* **Task 1.1:** Scaffold Go project structure:
  ```
  cmd/forum/
  cmd/forumctl/
  internal/storage/
  internal/merkle/
  internal/api/
  internal/pow/
  web/static/
  ```
* **Task 1.2:** Implement thread-name boundary parsing (`^[a-z]$`) and path builders targeting `threads/{t}.db`.
* **Task 1.3:** Build thread actor loop in Go (`runThreadWriter(ch)`). Implement message insertion and transaction wrapping.
* **Task 1.4:** Add automatic user version migrations and WAL pragma configurations on thread open.

## Milestone 2: Cryptographic Block System & Pruner
**Objective:** Implement RFC 6962-compliant Merkle trees, sliding-block storage budgets, and administrative tombstoning.
* **Task 2.1:** Write the binary leaf builder (`0x00` domain separator) and dynamic binary tree calculator (`0x01` separator).
* **Task 2.2:** Write the block sealer: index tracking, root generation, Ed25519 checkpoint signing, and timeout flush workers (30m trigger).
* **Task 2.3:** Implement block-level pruning loop triggered at 10 MB payload budget, updating `meta.chain_anchor`.
* **Task 2.4:** Build Unix Domain Socket administrative listener (`/run/forum/admin.sock`) and implement non-breaking tombstones.

## Milestone 3: Transport Layer, Anti-Spam & PoW Engine
**Objective:** Expose the complete public API surface, SSE event streaming, and adaptive cryptographic protections.
* **Task 3.1:** Implement request validation pipeline: raw byte UTF-8 checks, control-character stripping, line collapse, and size assertion.
* **Task 3.2:** Build identity engine: daily UTC salt rotation, poster ID derivation (`/64` truncated IPv6 support), and tripcode parser.
* **Task 3.3:** Build SSE broker subscribing directly to write commit hooks in the thread manager.
* **Task 3.4:** Build terminal endpoints (`GET /api/{t}.txt`) with continuation line indentation.
* **Task 3.5:** Implement stateless Hashcash engine: token generation, `HTTP 428` challenges, and `X-PoW` validation guards.

## Milestone 4: Frontend Implementation & Strict CSP
**Objective:** Build high-contrast, responsive client-side UI requiring zero third-party assets.
* **Task 4.1:** Write semantic HTML shell and dark-mode CSS utilizing CSS grid/flex, `100dvh` containment, and monospace typography.
* **Task 4.2:** Build single-file vanilla client script (<10 KB):
  * SSE connection with automatic back-off and polling fallback.
  * Tab visibility hooks to release network handles.
  * DOM generation via `document.createElement` / `.textContent` (strictly zero `innerHTML`).
  * CSS `:target` quote handler.
  * Sub-1000ms browser WebCrypto SHA-256 worker for PoW challenge resolution.

## Milestone 5: Verification, Benchmarking, & Release
**Objective:** End-to-end load testing, invariant validation, and packaging.
* **Task 5.1:** Concurrency stress test: 2,000 parallel workers posting to `/h/` to confirm zero `SQLITE_BUSY` crashes.
* **Task 5.2:** Pruning boundary validation: Inject 15 MB of message payload; verify storage settles gracefully within budget + one block (~10.13 MB max) with valid chain anchors.
* **Task 5.3:** Merkle verification tool: Write offline script to ingest `/api/{t}` JSON, rebuild Merkle roots from leaves, and confirm against the signed block headers.
* **Task 5.4:** Compile single static binary deployment via Go build flags (`CGO_ENABLED=1`). Produce systemd service units.