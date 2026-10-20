# Ephemeral Forum (`webfart`)

A high-throughput, minimalist, text-only discussion board combining the simplicity of early message boards with verifiable cryptographic auditability and isolated per-thread concurrency.

---

## Key Features & Architecture

* **Zero Sessions & Ephemeral Storage:** No user accounts, passwords, or cookies. Storage runs on an active tumbling-block ring buffer capped at 10 MB per thread.
* **Isolated File-Per-Thread Concurrency:** Each thread is an autonomous SQLite database (`threads/{thread}.db`) managed by a dedicated single-writer actor goroutine, eliminating write contention (`SQLITE_BUSY`) by design (>7,000 writes/sec).
* **Append-Only Merkle Integrity:** Messages are sealed into 128-message Merkle blocks (or after 30 minutes of inactivity) linked by a cryptographic SHA-256 hash chain and signed with persistent Ed25519 keys (RFC 6962 tree structure).
* **Deterministic Identity:** 
  * **Poster ID (`pid`)**: HMAC-SHA256 of IP (/64 for IPv6) and thread name, rotated automatically at 00:00 UTC.
  * **Optional Tripcodes**: `Nick#Secret` formatted nicks yield verifiable `Nick!trip` tripcodes without persisting the secret.
* **Dual-Citizen (Terminal & Web):**
  * **Terminal**: Browse via plain `curl`, plaintext ASCII continuation streams (`.txt`), and live Server-Sent Events (`/stream`).
  * **Web**: Dependency-free vanilla HTML5/CSS (<10 KB client script), strict CSP (`default-src 'none'`), pure dark mode with deterministic `pid` author tinting, smart auto-scroll, and inline quote (`>>seq`) targeting.
* **Adaptive Defense:** Standard 10-second IP rate limiting with adaptive Proof-of-Work (Hashcash challenge via HTTP 428 / WebCrypto) under high global load.

---

## Directory & Component Map

```
webfart/
├── bin/                       # Compiled release binaries
│   ├── forum                  # Main web & API daemon
│   ├── forumctl               # Admin CLI for tombstoning
│   └── merkleverify           # Offline Merkle tree audit tool
├── cmd/
│   ├── forum/                 # Server daemon entrypoint
│   ├── forumctl/              # Moderation CLI
│   └── merkleverify/          # Merkle verification CLI
├── internal/
│   ├── api/                   # HTTP router, validation, SSE, rate limiting, admin UDS
│   ├── identity/              # UTC daily salt, pid derivation, tripcodes
│   ├── merkle/                # RFC 6962 Merkle tree, block serialization, Ed25519 signing
│   ├── pow/                   # Stateless Hashcash generator & solver
│   └── storage/               # Per-thread SQLite actor, tumbling-block pruner, cache
├── systemd/                   # Hardened production service unit
└── web/static/                # Frontend assets (index.html, style.css, app.js <10 KB)
```

---

## Getting Started

### Prerequisites

* **Go 1.25+**
* **GCC** (for `CGO_ENABLED=1` SQLite WAL support)

### Build & Test

```bash
# Compile all binaries to bin/ (forum, forumctl, merkleverify)
make build

# Cross-compile for aarch64 (ARM64) to bin/aarch64/
make aarch64

# Run unit, integration, 2000-worker concurrency, and 15MB pruning tests
make test
```

### Running the Forum

```bash
./bin/forum -listen :8080 -data ./threads -static ./web/static
```

#### Server Options

| Flag | Default | Description |
|---|---|---|
| `-listen` | `:8080` | HTTP listen address |
| `-data` | `./threads` | Directory storing per-thread SQLite databases and keys |
| `-static` | `./web/static` | Path to static frontend assets |
| `-sock` | `/run/forum/admin.sock` | Path for administrative Unix domain control socket |
| `-rate-limit` | `10` | Minimum seconds between posts per IP address |
| `-pow-threshold` | `10` | Global post rate in 5 seconds to trigger Hashcash PoW |
| `-force-pow` | `false` | Force Hashcash Proof-of-Work challenge on all posts |

---

## API & Terminal Usage

### 1. Read Index

Returns pinned home-row threads (`a, s, d, f, g, h, j, k, l`) and any dynamic ad-hoc threads with posts in the trailing 24 hours:

```bash
curl -s http://localhost:8080/api/index
```

### 2. View Formatted Plaintext (Terminal Client)

Multi-line messages are indented with 2 spaces for readable terminal viewing:

```bash
curl -s http://localhost:8080/api/h.txt
```

### 3. Post a Message (`curl`)

```bash
curl -i -X POST \
  -H "Content-Type: application/json" \
  -d '{"nick": "alice#mypassword", "message": "Hello from the terminal!\n>>1 Testing quotes."}' \
  http://localhost:8080/api/h
```

### 4. Live Event Stream (SSE)

Subscribe to real-time committed posts with catch-up support:

```bash
curl -N -H "Last-Event-ID: 0" http://localhost:8080/api/h/stream
```

---

## Moderation & Tombstoning (`forumctl`)

Admin actions communicate with the running server via its local Unix Domain Socket. The thread's single-writer loop zeroes out `pid`, `nick`, `body`, and `nonce`, while permanently retaining `seq`, `created_at`, and the cryptographic `leaf` hash. Merkle root continuity remains unbroken.

```bash
# Tombstone message sequence 42 in thread /h/
./bin/forumctl tombstone -t h -s 42 -sock /run/forum/admin.sock
```

---

## Merkle Audit Tool (`merkleverify`)

Verify that the local thread database satisfies all RFC 6962 Merkle tree invariants, hash chain links, and Ed25519 signatures:

```bash
./bin/merkleverify -db ./threads/h.db [-pubkey <hex_public_key>]
```

Example output:
```text
Verifying thread /h/: found 2 sealed blocks...
  Block 0 [seq 1..128]: Merkle root and 128 leaves verified!
  Block 1 [seq 129..256]: Merkle root and 128 leaves verified!
All block headers, hash chains, and Merkle roots verified successfully!
```

---

## Production Deployment

### Option A: Bare Metal / VM (systemd)

A hardened systemd unit file is provided at [`systemd/forum.service`](systemd/forum.service):

```bash
# 1. Create system user
sudo useradd -r -s /usr/sbin/nologin forum

# 2. Install binaries and web assets
sudo cp bin/forum bin/forumctl bin/merkleverify /usr/local/bin/
sudo mkdir -p /usr/local/share/forum/web/static
sudo cp web/static/* /usr/local/share/forum/web/static/

# 3. Create persistent data directory
sudo mkdir -p /var/lib/forum/threads
sudo chown -R forum:forum /var/lib/forum

# 4. Install and start systemd service
sudo cp systemd/forum.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now forum
```

### Option B: Docker / Container

```bash
# Build and run with Docker Compose
docker compose up -d
```

### Reverse Proxy (Caddy or Nginx)

For HTTPS and live SSE streams, terminate TLS with Caddy or Nginx.

**Caddyfile:**
```caddy
forum.yourdomain.com {
    reverse_proxy localhost:8080
}
```

**Nginx:**
```nginx
server {
    server_name forum.yourdomain.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;

        # Disable buffering for Server-Sent Events (SSE)
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Connection '';
        proxy_http_version 1.1;
        chunked_transfer_encoding off;
        proxy_read_timeout 24h;
    }
}
```
