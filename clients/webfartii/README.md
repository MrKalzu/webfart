# webfartii

A command-line client for [webfart](../..) in the spirit of suckless.org's [ii](https://tools.suckless.org/ii/): a small daemon turns each thread into a directory with an `in` FIFO and an append-only `out` file, so you can read with `tail -f` and post with `echo`. It also has one-shot commands for quick reads and posts.

* Single file, Python 3.8+, **standard library only**
* No accounts, no dependencies, no config files
* Handles SSE streaming with automatic reconnect and resume, rate limiting (`429`), and adaptive Proof-of-Work (`428`) transparently

## Install

```bash
chmod +x webfartii.py
sudo cp webfartii.py /usr/local/bin/webfartii     # optional
```

## Configuration

| Flag | Environment | Default | Description |
|---|---|---|---|
| `-s`, `--server` | `WEBFART_URL` | `http://localhost:8080` | Server base URL (`http://` is assumed if omitted) |
| `-n`, `--nick` | `WEBFART_NICK` | *(anonymous)* | Nick, or `nick#secret` for a tripcode |

Global flags go **before** the subcommand: `webfartii -n 'alice#pw' post h 'hi'`.

Threads are single lowercase letters, `a` to `z`.

```bash
export WEBFART_URL=https://forum.example.com
export WEBFART_NICK='alice#secret'
```

## One-shot commands

```bash
webfartii index                       # active threads, head_seq, time of last post
webfartii cat h                       # plaintext view of /h/ (GET /api/h.txt)
webfartii tail h                      # follow /h/ live: history first, then new posts
webfartii tail h --new                # only new posts

webfartii post h 'hello there'        # post a message
echo 'from a pipe' | webfartii post h
```

Multi-line messages: with `post`, whatever you pipe in is sent as one message with its newlines intact.

```bash
printf 'line one\nline two\n>>1 quote' | webfartii post h

webfartii post h <<'EOF'
first line
second line
EOF
```

`post` exits 0 on success and 1 on failure, so it is safe to use in scripts.

## ii mode

```bash
webfartii ii h a            # run the daemon, joining /h/ and /a/ at startup
```

The daemon keeps a directory tree under `~/webfart/<server>/` (change it with `ii -p PREFIX`). `<server>` is the host (and port) from the URL, for example `forum.example.com` or `localhost_8080`.

```
~/webfart/<server>/in             control FIFO
~/webfart/<server>/out            server-level log: joins, index results, errors
~/webfart/<server>/<thread>/in    write a line here to post it
~/webfart/<server>/<thread>/out   append-only log of the thread
~/webfart/<server>/<thread>/.seq  last seq seen, so restarts resume without duplicates
```

```bash
tail -f ~/webfart/forum.example.com/h/out
echo 'hello world' > ~/webfart/forum.example.com/h/in
echo '/j x' > ~/webfart/forum.example.com/in
```

### Input

Each line written to a thread's `in` is one message.

| Input | Effect |
|---|---|
| `some text` | Posted as a message |
| `line one\nline two` | A literal backslash-n becomes a newline, giving a multi-line message |
| `//text` | Posted as the literal text `/text` |
| `/n nick#secret` | Change nick (the secret is never written to any `out` file) |
| `/l` | Leave this thread |

> A real newline written to the FIFO ends the message, so `printf 'a\nb\n' > in` sends two posts. To send one multi-line message, write a literal backslash-n: `echo 'a\nb' > in` (plain `echo`, no `-e`).

The control FIFO (`<server>/in`) accepts:

| Command | Effect |
|---|---|
| `/j h a x` | Join threads |
| `/l h` | Leave a thread |
| `/i` | Write the index to the server-level `out` |
| `/n nick#secret` | Change nick |
| `/q` | Quit |

Options for `ii`: `-p PREFIX` sets the directory root, and `--no-history` skips asking the server to replay a thread's history on the first join (resumed joins always continue from `.seq`).

### Output format

```
2026-10-20 12:33:45     1 <alice!k3Fx9a@5562fd31> first line
  second line
2026-10-20 12:33:46     2 <5562fd31> posted without a nick
2026-10-20 12:33:47 -!- rate limited, retrying in 10s
```

Each line is: local timestamp, `seq`, `<nick@pid>`, then the message. Continuation lines of multi-line posts are indented by two spaces. Anonymous posts show only the pid. Tombstoned posts show as `[tombstoned]`. Lines marked `-!-` are client notices, such as errors and retries.

### Running as a service

A minimal systemd user unit, for example `~/.config/systemd/user/webfartii.service`:

```ini
[Unit]
Description=webfartii

[Service]
Environment=WEBFART_URL=https://forum.example.com
ExecStart=/usr/local/bin/webfartii ii h
Restart=on-failure

[Install]
WantedBy=default.target
```

With that running, any script can post by writing a line to a FIFO, which makes it handy for notifications:

```bash
echo "backup finished" > ~/webfart/forum.example.com/h/in
```

## Behaviour worth knowing

* **Limits are checked locally first.** Messages over 1024 bytes or under 2 non-whitespace characters, and nicks over 32 ASCII characters (counting the `#secret`), are rejected before any request, with the reason written to `out` (or stderr for `post`).
* **Rate limiting.** On `429` the client waits for `Retry-After` and retries. In ii mode this queues input rather than dropping it.
* **Proof-of-Work.** On `428` the client solves the Hashcash challenge and retries with the `X-PoW` header, automatically.
* **Terminal safety.** Control characters and bidi overrides in received messages are stripped, so a poster cannot inject escape sequences into your terminal.
* **Reconnects.** The SSE stream reconnects with backoff, resuming from the last seq seen. If a thread's database is ever reset on the server (seq restarts from 1), delete `<thread>/.seq`.
* **Tripcode secrets.** `-n 'nick#secret'` is visible in the process list and your shell history. Prefer `WEBFART_NICK` from a file with tight permissions. The ii directories are created `0700` and the FIFOs `0600`.
* **Platform.** ii mode needs a Unix with `mkfifo`. The one-shot commands work anywhere Python does.
