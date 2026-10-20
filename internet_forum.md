# Minimalist Ephemeral Forum Specification (v1.0)

## 1. Overview & Core Philosophy
A lightweight, text-only discussion board bridging the gap between IRC and classic anonymous imageboards. 
- **No accounts, no sessions, no edits.**
- **Storage:** Fixed 10 MB FIFO ring-buffer per thread.
- **Backend:** Go (standard library + lightweight router) + SQLite (in WAL mode).
- **Frontend:** Plain semantic HTML5 + CSS (Dark mode), <150 lines of vanilla JavaScript.

---

## 2. Thread Architecture & Namespace

### 2.1 Thread Subpaths
* Threads are identified by a single ASCII letter (`^[a-z]$`, case-insensitive, normalized to lowercase), allowing **26 total possible threads**.
* **Default Index Threads:** `a`, `s`, `d`, `f`, `g`, `h`, `j`, `k`, `l` (the keyboard home row).
* **Ad-hoc Threads:** The remaining letters (`b`, `c`, `e`, etc.) exist dynamically. Querying or posting to them instantiates or reads them without special administrative creation.

---

## 3. Data Model & SQLite Schema

### 3.1 SQLite Schema
```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,             -- UUIDv4 string
    thread CHAR(1) NOT NULL,          -- 'a' through 'z'
    nickname VARCHAR(32) NOT NULL,    -- Provided nick or short SHA hash
    message TEXT NOT NULL,            -- UTF-8 message content (max 1024 bytes)
    byte_size INTEGER NOT NULL,       -- Stored payload size in bytes
    created_at INTEGER NOT NULL       -- Millisecond Unix epoch (e.g. 1718000000123)
);

CREATE INDEX IF NOT EXISTS idx_thread_created 
ON messages (thread, created_at DESC);

-- Cache table to maintain ring-buffer budget efficiently
CREATE TABLE IF NOT EXISTS thread_storage (
    thread CHAR(1) PRIMARY KEY,
    total_bytes INTEGER DEFAULT 0
);
```

### 3.2 10 MB FIFO Ring Buffer Implementation
Each thread allows a maximum payload size of **10,485,760 bytes (10 MB)**.
When inserting a new message:
1. Run inside an SQLite transaction.
2. Increment `total_bytes` in `thread_storage`.
3. If `total_bytes > 10,485,760`:
   ```sql
   -- Find and prune oldest records until budget is satisfied
   WITH OldestMessages AS (
       SELECT id, byte_size 
       FROM messages 
       WHERE thread = ? 
       ORDER BY created_at ASC
   )
   -- Prune loop removes rows and subtracts byte_size until within limit.
   ```
4. Insert the new message.

---

## 4. Identity, Rate Limiting & Fingerprinting

### 4.1 Nickname Generation
* **Field optional:** If blank, generate a short hash:
  $$\text{nickname} = \text{SHA256}(\text{message\_bytes})[0:8]$$
* **If provided:** Stripped of control characters, max 32 ASCII characters.

### 4.2 Fingerprinting & Anti-Spam
* **No cookies or local storage.**
* **Fingerprint:** Server-side SHA-256 hash of:
  $$\text{Client IP} + \text{User-Agent} + \text{Server Secret Salt}$$
* **Rate Limiter:** In-memory Go cache (e.g., `sync.Map` with automated TTL eviction).
* **Throttle Rule:** Strictly **1 post per 10 seconds** per fingerprint. Violations return `HTTP 429 Too Many Requests`.

---

## 5. API Endpoints

All data transfer uses JSON over HTTP/HTTPS.

### 5.1 `GET /api/index`
Returns default home-row threads (`a, s, d, f, g, h, j, k, l`) and their latest 5 messages.

**Response:**
```json
{
  "threads": {
    "a": [
      {
        "id": "c7a8b606-f131-409b-8be2-723145455648",
        "nickname": "anon-4a8b",
        "message": "hello world",
        "created_at": 1718000000123
      }
    ]
  }
}
```

### 5.2 `GET /api/{thread:[a-z]}`
Fetches messages for a specific thread. Supports cursor pagination.

* **Query Params:**
  * `limit`: (Default `50`, Max `100`).
  * `since`: Millisecond timestamp to fetch messages newer than cursor (for polling).

**Response:**
```json
{
  "thread": "a",
  "storage_used_bytes": 482100,
  "messages": [
    {
      "id": "0d20d771-4246-4cb4-a1db-f9754f76269b",
      "nickname": "b8a32f10",
      "message": "Minimalist text board.",
      "created_at": 1718000010450
    }
  ]
}
```

### 5.3 `POST /api/{thread:[a-z]}`
Post a new message.

* **Payload Constraints:**
  * `message`: Non-empty, $\le 1024$ bytes.
  * `nickname`: Optional, $\le 32$ ASCII characters.

**Request:**
```json
{
  "nickname": "retro_coder",
  "message": "The terminal aesthetic feels right."
}
```

**Responses:**
* `201 Created`: Message accepted and persisted.
* `400 Bad Request`: Message empty, > 1024 bytes, or invalid nick format.
* `429 Too Many Requests`: Rate limit active (<10s since last post).

---

## 6. Frontend & UI/UX Specification

### 6.1 Layout & Visual Style
* **Theme:** High-contrast Dark Mode.
  * Background: `#0f0f10`
  * Text: `#e0e0e0`
  * Borders/Separators: `#222225`
  * Monospaced/System UI fonts (`ui-monospace, "Cascadia Code", monospace`).
* **Layout Structure:**
  * **Top Bar:** Navigation pills linking to default threads (`/a/`, `/s/`, `/d/`, etc.).
  * **Middle Area:** Scrollable vertical message stream, sorted chronologically with auto-scroll down for new incoming messages.
  * **Fixed Bottom Bar:** Clean form bar pinned to the bottom containing:
    * Nickname input (width: ~140px).
    * Message input (remaining flex width).
    * Submit button / `Enter` key shortcut.

```
+-----------------------------------------------------------+
| [a] [s] [d] [f] [g] [h] [j] [k] [l]   Thread: /h/         |
+-----------------------------------------------------------+
|                                                           |
|  [21:40:12.100] <a94f08c3> Hello from the terminal       |
|  +-----------------------------------------------------+  |
|  | Message bubble tinted with hsl(120, 25%, 12%)       |  |
|  +-----------------------------------------------------+  |
|                                                           |
+-----------------------------------------------------------+
| [ Nick (optional) ] [ Message text...               ] [>] |
+-----------------------------------------------------------+
```

### 6.2 Deterministic Nickname Tinting
Message bubbles receive a subtle background tint derived deterministically from the author's nickname:

```javascript
function getNickColor(nick) {
  let hash = 0;
  for (let i = 0; i < nick.length; i++) {
    hash = nick.charCodeAt(i) + ((hash << 5) - hash);
  }
  const hue = Math.abs(hash % 360);
  // Low saturation (20%) and low lightness (12%) for a subtle dark tint
  return `hsl(${hue}, 20%, 12%)`;
}
```

### 6.3 Minimalist JavaScript Scope
Total client script footprint: under 3 KB uncompressed.
1. Form submit handler via `fetch()` (prevents full-page reload).
2. Interval polling (fetch newer messages via `?since=<last_timestamp>` every 3–5 seconds).
3. Client-side XSS safety: render all dynamic content using `.textContent`.
4. Storage: **Zero** cookies, zero `localStorage` (nicknames reset on page refresh).

---

## 7. Security & Edge Considerations

1. **Strict HTML Escaping:** Because messages are arbitrary user text, all output must be strictly rendered as text (never `innerHTML`) to prevent Cross-Site Scripting.
2. **UTF-8 Byte Boundary Slicing:** Message length must be checked in byte size (`len([]byte(msg)) <= 1024`) rather than rune count to ensure the 1 KB boundary cannot be bypassed with multi-byte unicode characters.
3. **Database Concurrency:** SQLite in WAL mode handles multiple concurrent readers and a single serialized writer without lock contention at this scale.