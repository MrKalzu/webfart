# Ephemeral Forum: Block-Based Merkle Design
Oct 19, 2026 · @Kalzu

## Overview
Small sealed blocks of about 128 messages solve the storage-overhead problem without one big tree. The overhead was never the mountain size; it was persisting interior nodes. A Merkle tree over n leaves has roughly n interior nodes whatever its shape, so a small block lets you skip storing them and recompute them on demand.
What remains per message is a leaf hash and a nonce, about 48 B. A hash chain over block headers then gives ordering and tamper-evidence, which is what a replica needs to take over if the original server dies.
Block structure
Every 128 messages seal into a block. 128 rather than 100 only so block math is seq >> 7; RFC 6962-style tree hashing handles any count, so 100 works too.
Layer
What is stored
Size
Message
Leaf hash and nonce; interior nodes are never persisted
about 48 B per message
Block header
block_idx, first_seq, count, merkle_root, prev_header_hash
about 100 B per 128 messages
Checkpoint
Signature over the latest header hash
one per sealed block
A proof request recomputes the interior nodes from at most 128 leaves, which takes microseconds.
Window and pruning
The "sliding window" is really tumbling blocks. A root recomputed over the last 100 posts on every insert is not append-only and commits to no history, so it is useless to a replica.
Instead, each message lives in exactly one block, and the window slides by dropping the oldest whole block once the thread exceeds 10 MB.
• The budget overshoots by at most one block, about 128 KB worst case, roughly 1%.
• The dropped block's header hash becomes the new chain anchor.
The open block
The newest block holds up to 127 unsealed messages. Seal it on count or on a timer, recording count in the header, so quiet threads still get checkpoints.
Replicas apply messages as they arrive and verify the root when the block seals.
Storage: one SQLite file per thread
Keep SQLite, with one file per thread (threads/h.db). The block design needs nothing SQLite cannot do, and a custom segment format would mostly reimplement a WHERE seq > ? ORDER BY seq LIMIT ? query.
• Simpler schema. The thread column and the thread_storage table disappear, because the file is the thread. The rowid is the per-thread seq.
• Cheap lifecycle. Ad-hoc threads open their file on first touch, wiping a thread means deleting a file, and a corrupt file takes out only one thread.
• No write contention. One writer goroutine owns one connection per thread, so SQLITE_BUSY is gone by construction.
• Natural replication unit. One thread is one DB and one chain.
-- threads/h.db
CREATE TABLE messages (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  pid        TEXT,          -- poster ID; NULL once tombstoned
  nick       TEXT,          -- NULL once tombstoned
  body       BLOB,          -- NULL once tombstoned
  nonce      BLOB,          -- NULL once tombstoned
  created_at INTEGER NOT NULL,
  leaf       BLOB NOT NULL  -- 32 B, kept forever
);
CREATE TABLE blocks (
  idx INTEGER PRIMARY KEY, first_seq INTEGER NOT NULL, count INTEGER NOT NULL,
  root BLOB NOT NULL, prev BLOB NOT NULL, sig BLOB NOT NULL
);
CREATE TABLE meta (k TEXT PRIMARY KEY, v);  -- chain anchor, byte total, epoch
Pruning one block at a time
A per-thread file makes pruning a whole block of 128 messages a single transaction. When the byte total in meta exceeds the budget, one transaction deletes the oldest sealed block's messages, subtracts their size and stores that block's header hash as the chain anchor. Repeat until the thread is under budget, and never prune the open block. The header row itself can stay, at about 100 B per block.
Freed pages go back to SQLite's freelist and are reused by later inserts, so the file plateaus near the budget with no VACUUM. Set PRAGMA max_page_count (for example 32 MB) as a hard physical cap.
Operational notes
• Shared state lives outside the thread files. Keep the signing key and master secret in config. Derive the poster-ID salt as HMAC(master, date), so it needs no table and rotation is free. The root-of-roots, if kept, is computed from each thread's latest header.
• Back up with the backup API or VACUUM INTO. Copying a live WAL-mode file is a classic foot-gun. Litestream per file also works.
• Replicate logically, not by file. A page-level copy proves nothing and ties the replica to the primary's layout. The replica pulls headers and blocks over the API and verifies roots and the chain, and its own layout can differ (retaining everything, say). Raw file copies are for backups only.
• Migrations run per file. Set PRAGMA user_version and loop over the files at open.
• /api/index becomes 9 small queries across 9 DBs. Cache the latest 5 per thread in memory and update on post.
Replication and failover
A hash chain over block headers is enough for replication, since replication needs ordering plus tamper-evidence. Headers are tiny (a few hundred per thread), so a replica can pull headers from one place and content from another, and verify locally.
• Tombstones never touch sealed blocks. Deleting zeroes the content and nonce but keeps the leaf hash, so roots do not change. Replicas need only a delete log, not a re-sync. Deletion on a mirror is cooperative: a replica that keeps the content and nonce can still prove the message existed.
• The replica can retain more than the primary. It does not have to prune at 10 MB, so it can be the archive while the primary stays ephemeral.
• Failover needs two secrets on the replica: the signing key and the poster-ID salt. If the key died with the primary, the new primary signs an explicit handover ("epoch 2, continues from head X"). Trust in the new key has to be established out of band, for example by publishing it somewhere you control.
Chain or MMR on top?
Start with the chain. Its one weakness is that proving an old message against a fresh checkpoint means walking headers forward, tens of KB worst case.
If you later want cheap third-party receipts, put an MMR over the block roots. The bottom layer is identical, so it is a drop-in, and with n/128 leaves its storage overhead is negligible. Dropping the oldest block is also a left-side prune, which is the one thing MMRs do well.
In short, this is a hash chain of Merkle-rooted blocks: a blockchain without the electricity bill.
Hardening and UX
Everything here is settled unless it also appears under Open questions at the end.
Identity and API shape
• seq is the message identity. UUIDs are dropped from the schema and the API. The JSON id becomes the integer seq and the poll cursor is since_seq. Responses are ascending, and when more than limit messages are newer than the cursor, the server returns the oldest ones after it.
• The thread letter is now a filename, so it is validated first. Lowercase it, match ^[a-z]$, and build the path from that single validated byte, never from the request string. A GET for a thread with no file returns an empty list and creates nothing. Only the first successful POST creates the file.
• Poster ID replaces the hash-of-message nick. pid = HMAC(daily_salt, ip ‖ thread)[0:8], where daily_salt = HMAC(master, UTC date) and IPv6 addresses are truncated to /64. The ID is stable within one thread for one UTC day. Bubble tints derive from it, so a tint shows who is talking to whom. A blank nick displays the ID alone. The ID is stored in a pid column and is part of the leaf hash.
• Optional tripcodes. A nick of the form nick#secret is split at the first #. The server stores nick!trip, where trip = base64url(HMAC(trip_key, secret))[0:8]. The secret is never stored and is not part of the leaf. trip_key is a stable secret in config and is replicated with the signing key. The secret travels in the POST body, so serve over HTTPS only.
Validation and normalization
Everything before the final step runs before hashing, so the leaf always covers exactly the stored bytes.
1. Require Content-Type: application/json, else 415. This forces a CORS preflight for cross-origin pages, so a random site cannot make visitors' browsers post here.
2. Check that the raw request body is valid UTF-8, else 400. Go's JSON decoder silently repairs invalid UTF-8, so this check must run on the raw bytes before decoding.
3. Decode the JSON and normalize line endings to \n.
4. Strip NUL, all other control characters except \n, and bidi override and isolate characters.
5. Trim leading and trailing whitespace.
6. Require at least 4 non-whitespace characters (Unicode code points), else 400.
7. Collapse any run of more than 3 empty lines to a single empty line.
8. Require at most 1024 bytes (len([]byte(msg))), measured after steps 4, 5 and 7, else 400.
9. Build the leaf and store. The leaf is a hash of a length-prefixed binary encoding of (thread, seq, pid, nick, created_at, nonce, body) with a 0x00 domain-separation prefix. Interior nodes use 0x01.
Rate limiting
The limit of 1 post per 10 seconds from the original spec stays, answered with 429 and a Retry-After header. The limiter's client key is the IP alone, with IPv6 truncated to /64, and the User-Agent plays no part. The app trusts X-Forwarded-For only when the request comes from the proxy's address. Hashcash is added on top as an adaptive guard, not a default. It defeats IP rotation, which the per-IP limit does not.
1. The client calls GET /api/challenge and receives a stateless token: a timestamp, a difficulty and an HMAC signature, so the server stores nothing.
2. The client finds a counter such that SHA-256(token ‖ counter) has d leading zero bits.
3. The client posts with X-PoW: <token>:<counter>. The server checks the signature, a freshness window of about 60 seconds, and that the token has not been used before.
A plain curl loop in bash is too slow to solve this. Spawning sha256sum per attempt manages only hundreds to low thousands of hashes per second, so 16 bits (about 65,000 tries) takes a minute or more. A small helper, such as a static Go binary or a short Python script, does millions per second and solves it in well under a second.
So the guard is adaptive: no proof of work in normal conditions, so curl --json works as is. When the server is under pressure (for example the global post rate is over a threshold), POSTs without a valid proof get 428 with the challenge in the body, and a client can solve it and retry. The browser solves it with crypto.subtle, with difficulty tuned so it takes about a second.
Moderation and logging
• Admin CLI deletes by (thread, seq). The CLI talks to the running server over a local unix socket, and the thread's writer goroutine performs the tombstone. The single-writer rule holds and the CLI never opens the DB file. A tombstone zeroes pid, nick, body and nonce, keeps the leaf, and appends to the deletion log that replicas apply.
• The application keeps no request logs. Access logging is the job of the web front-end (WFE) proxy, and its retention is outside this design.
Frontend
• The 201 returns the created message. It carries its seq, and the client dedupes by seq, so a post appears at once instead of at the next poll.
• Auto-scroll only when the reader is at the bottom. Otherwise show a "new messages" pill.
• Pause when hidden. On visibilitychange, close the stream (or stop polling) and resume on return from the last seen seq.
• Use 100dvh for the pinned bottom bar, so mobile keyboards do not cover it.
• Nick in the URL fragment. /h/#retro_coder pre-fills the nick input from location.hash. Fragments are never sent to the server and nothing is stored, so the zero-cookie, zero-localStorage rule holds. Put only the name there: a tripcode secret in the URL would end up in browser history.
• >>seq quoting. The client turns >>4812 into a link to the message element #m4812 in the same thread, built with DOM nodes and .textContent, never innerHTML. Highlighting uses the CSS :target rule, so it needs no script. A quote of a message that is not on the page stays plain text.
• CSP. Send Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'. The script lives in its own file with no inline code. style-src 'self' blocks inline style attributes, so set tints from script with el.style.background, which CSP permits.
• Script budget. The total client script stays under 10 KB uncompressed, with no build step, so what is written is what ships. The original 3 KB and 150-line limits are retired.
Terminal clients
• GET /api/{thread}.txt returns plain text (text/plain; charset=utf-8 with X-Content-Type-Options: nosniff), takes the same limit and since_seq parameters, and indents continuation lines of multi-line messages.
• GET /api/{thread}/stream is Server-Sent Events. The thread's writer goroutine publishes each committed message to that thread's subscribers. Each event carries id: <seq>, so a reconnecting client sends Last-Event-ID and resumes without gaps. The browser uses the stream, with ?since_seq= polling as the fallback. Cap concurrent streams per IP and per thread.
• Posting from curl is curl --json '{"message":"hello world"}' https://host/api/h (curl 7.82 or newer). On older curl, add -H 'Content-Type: application/json' to -d. A bare -d sends form encoding and gets 415.
Homepage
The nine default threads (a s d f g h j k l) are always listed. Any other thread is listed while it has had a post in the last 24 hours. GET /api/index returns both groups with the latest 5 messages each, served from an in-memory cache updated on every post.