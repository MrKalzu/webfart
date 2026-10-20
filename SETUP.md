# Production Setup & Deployment Guide

This guide details how to install, configure, secure, and run the Ephemeral Forum (`webfart`) in production.

---

## Table of Contents
1. [Prerequisites](#1-prerequisites)
2. [Method A: Native Linux Service (systemd)](#2-method-a-native-linux-service-systemd)
3. [Method B: Container Deployment (Docker / Compose)](#3-method-b-container-deployment-docker--compose)
4. [Reverse Proxy & SSL Configuration](#4-reverse-proxy--ssl-configuration)
   - [Caddy (Recommended)](#caddy-automatic-https)
   - [Nginx](#nginx)
5. [Operational Management & Moderation](#5-operational-management--moderation)
6. [Backups & Verification](#6-backups--verification)
7. [Troubleshooting & FAQ](#7-troubleshooting--faq)

---

## 1. Prerequisites

### Build Environment
* **Linux** (Debian, Ubuntu, Fedora, Arch, Alpine, etc.)
* **Go 1.25+**
* **GCC / build-essential** (required for `CGO_ENABLED=1` SQLite WAL support)
* **Make**

To install build dependencies on Debian/Ubuntu:
```bash
sudo apt update
sudo apt install -y build-essential curl git
```

---

## 2. Method A: Native Linux Service (systemd)

This is the recommended deployment method for single VPS / bare metal hosts. It uses systemd hardening, sandbox isolation, and local Unix domain socket access.

### Step 1: Build Binaries
```bash
make build
```
This produces three static binaries in `bin/`:
- `bin/forum` — Main HTTP API daemon
- `bin/forumctl` — Administrative tombstone CLI
- `bin/merkleverify` — Offline Merkle tree audit tool

### Step 2: Create Dedicated System User
```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/forum forum
```

### Step 3: Install Binaries & Web Assets
```bash
# Install executables to system PATH
sudo cp bin/forum bin/forumctl bin/merkleverify /usr/local/bin/

# Install web frontend templates and assets
sudo mkdir -p /usr/local/share/forum/web/static
sudo cp web/static/* /usr/local/share/forum/web/static/
```

### Step 4: Configure Data Directories & Permissions
```bash
# Persistent directory for SQLite databases and cryptographic keys
sudo mkdir -p /var/lib/forum/threads
sudo chown -R forum:forum /var/lib/forum
sudo chmod 700 /var/lib/forum
```

### Step 5: Install & Start systemd Service
Copy the hardened service unit provided in the repository:
```bash
sudo cp systemd/forum.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now forum
```

### Step 6: Verify Operation
```bash
# Check status
sudo systemctl status forum

# Follow live service logs
sudo journalctl -u forum -f
```

---

## 3. Method B: Container Deployment (Docker / Compose)

If you prefer containerized deployment, a multi-stage `Dockerfile` and `docker-compose.yml` are provided.

### Start with Docker Compose
```bash
# Build and run container in detached mode
docker compose up -d

# Check live logs
docker compose logs -f
```

Data will be automatically persisted in `./threads_data/` on the host.

---

## 4. Reverse Proxy & SSL Configuration

Because the forum streams live real-time messages via **Server-Sent Events (SSE)**, buffering must be disabled in your reverse proxy.

### Caddy (Automatic HTTPS)

Caddy handles SSL certificates automatically and proxies SSE streams without buffering out of the box.

Create or edit `/etc/caddy/Caddyfile`:
```caddy
forum.yourdomain.com {
    reverse_proxy localhost:8080
}
```

Reload Caddy:
```bash
sudo systemctl reload caddy
```

---

### Nginx

Create `/etc/nginx/sites-available/forum`:

```nginx
server {
    server_name forum.yourdomain.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;

        # Mandatory SSE configuration: disable buffering
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Connection '';
        proxy_http_version 1.1;
        chunked_transfer_encoding off;
        proxy_read_timeout 24h;
    }

    listen 80;
}
```

Enable site and acquire SSL via Certbot:
```bash
sudo ln -s /etc/nginx/sites-available/forum /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
sudo certbot --nginx -d forum.yourdomain.com
```

---

## 5. Operational Management & Moderation

### Moderating Messages (`forumctl`)
To remove spam or illegal material without corrupting the Merkle block root or breaking replica hash chains, use `forumctl`:

```bash
# Tombstone message sequence 42 in thread /h/
forumctl tombstone -t h -s 42
```

The tombstoned message's `pid`, `nick`, `body`, and `nonce` will be zeroed out in the database, while the cryptographic `leaf` hash and `seq` are preserved for Merkle continuity.

### Adjusting Rate Limits & Anti-Spam
Edit `/etc/systemd/system/forum.service` to append flags:
```ini
ExecStart=/usr/local/bin/forum \
  -listen :8080 \
  -rate-limit 10 \
  -pow-threshold 10 \
  -data /var/lib/forum/threads \
  -static /usr/local/share/forum/web/static \
  -sock /run/forum/admin.sock
```
- `-rate-limit <seconds>`: Minimum interval per IP between posts (default: `10`).
- `-pow-threshold <count>`: Number of global posts in 5 seconds to activate adaptive Hashcash challenge (default: `10`).
- `-force-pow`: Set to true to enforce Hashcash Proof-of-Work on all posts (useful during DDoS).

Reload after editing:
```bash
sudo systemctl daemon-reload
sudo systemctl restart forum
```

---

## 6. Backups & Verification

### Verifying Thread Integrity (`merkleverify`)
Run offline audits against any thread's SQLite database to verify all RFC 6962 Merkle tree invariants and Ed25519 signatures:

```bash
merkleverify -db /var/lib/forum/threads/h.db
```

### Safe Backups
Because SQLite runs in **WAL mode**, never copy active WAL files directly with `cp` while the daemon is running. Use SQLite's backup API or `VACUUM INTO`:

```bash
# Create safe point-in-time snapshot
sqlite3 /var/lib/forum/threads/h.db ".backup /var/backups/forum/h_$(date +%Y%m%d).db"
```

---

## 7. Troubleshooting & FAQ

#### Q: How do I test with `curl`?
```bash
# Post a message
curl -i -X POST -H 'Content-Type: application/json' \
  -d '{"nick": "alice#secret", "message": "Deploy verified!"}' \
  https://forum.yourdomain.com/api/h

# View terminal stream
curl -s https://forum.yourdomain.com/api/h.txt
```

#### Q: Why does `/run/forum/admin.sock` fail with permission denied?
Ensure the systemd service has `RuntimeDirectory=forum` set, or pass `-sock /var/lib/forum/threads/admin.sock` if running as a non-systemd process.
