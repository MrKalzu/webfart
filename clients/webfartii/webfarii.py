#!/usr/bin/env python3
"""webfartii - ii-style, filesystem-driven client for the webfart ephemeral forum.

Standard library only. ii mode needs a Unix with mkfifo.

Layout (ii mode):

    <prefix>/<server>/in              control FIFO: /j thread, /l thread, /i, /n nick#secret, /q
    <prefix>/<server>/out             server-level log (joins, index, errors)
    <prefix>/<server>/<thread>/in     write a line here and it gets posted
    <prefix>/<server>/<thread>/out    append-only log of the thread (tail -f it)
    <prefix>/<server>/<thread>/.seq   last seq seen, so restarts resume without dupes

Examples:

    webfartii -n 'alice#secret' ii h a              # daemon, joins /h/ and /a/
    tail -f ~/webfart/localhost_8080/h/out
    echo 'hello world' > ~/webfart/localhost_8080/h/in
    echo '/j x' > ~/webfart/localhost_8080/in       # join another thread

    webfartii cat h                                 # GET /api/h.txt
    webfartii tail h                                # follow the SSE stream on stdout
    echo 'hi' | webfartii -n alice post h           # one-shot post
    webfartii index

Input lines in a thread's `in`:
    plain text   posted as a message (a literal \\n becomes a newline)
    //text       posted as "/text"
    /n nick#pw   change nick (tripcode secret is never written to any out file)
    /l           leave the thread
"""

import argparse
import hashlib
import json
import os
import re
import signal
import socket
import stat
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime

UA = "webfartii/0.1"
TS_FMT = "%Y-%m-%d %H:%M:%S"
STREAM_TIMEOUT = 90  # seconds of silence before we reconnect (resumes via Last-Event-ID)
THREAD_RE = re.compile(r"^[a-z]$")  # server: threads are single letters a-z
# control chars (except \n and \t), C1, and bidi overrides: never let the forum drive our terminal
_CTRL_RE = re.compile("[\x00-\x08\x0b-\x1f\x7f-\x9f\u202a-\u202e\u2066-\u2069]")


def clean(s):
    return _CTRL_RE.sub("", str(s))


def now():
    return datetime.now().strftime(TS_FMT)


def to_int(v):
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


# --------------------------------------------------------------------------- formatting

def fmt_ts(v):
    try:
        if isinstance(v, str):
            try:
                v = float(v)
            except ValueError:
                return datetime.fromisoformat(v.replace("Z", "+00:00")).astimezone().strftime(TS_FMT)
        if isinstance(v, (int, float)):
            if v > 1e14:
                v /= 1e6
            elif v > 1e11:
                v /= 1e3
            return datetime.fromtimestamp(v).strftime(TS_FMT)
    except (ValueError, OverflowError, OSError):
        pass
    return now()


def format_event(obj, seq=None):
    """One forum message -> list of output lines (continuation lines indented)."""
    if not isinstance(obj, dict):
        return ["%s %5s %s" % (now(), "-", clean(obj))]
    seq = obj.get("seq", seq)
    body = obj.get("body", obj.get("message", obj.get("text", ""))) or ""
    nick = obj.get("nick") or ""
    pid = obj.get("pid") or ""
    ts = fmt_ts(obj.get("created_at", obj.get("ts")))
    if not (body or nick or pid):
        who, body = "-", "[tombstoned]"
    else:
        if not nick or nick == pid:  # server stores the pid as nick for anonymous posts
            who = clean(pid)[:8] or "anon"
        else:
            who = clean(nick).split("#", 1)[0]  # never display a tripcode secret
            if pid:
                who += "@" + clean(pid)[:8]
    lines = clean(body).split("\n")
    out = ["%s %5s <%s> %s" % (ts, "-" if seq is None else seq, who, lines[0])]
    out += ["  " + l for l in lines[1:]]
    return out


def format_index(text):
    """/api/index -> {"active_threads": [...], "threads": {t: {"head_seq", "latest": [msg...]}}}"""
    try:
        j = json.loads(text)
    except ValueError:
        return [clean(l) for l in text.splitlines()]
    if isinstance(j, dict) and isinstance(j.get("threads"), dict):
        out = []
        for t in j.get("active_threads") or sorted(j["threads"]):
            s = j["threads"].get(t) or {}
            line = "%s head_seq=%s" % (t, s.get("head_seq", 0))
            latest = s.get("latest") or []
            if latest and isinstance(latest[-1], dict):
                line += " last=%s" % fmt_ts(latest[-1].get("created_at"))
            out.append(clean(line))
        return out
    return [clean(json.dumps(j))]


# --------------------------------------------------------------------------- HTTP / SSE

class Api:
    def __init__(self, base):
        self.base = base

    def _open(self, path, data=None, headers=None, timeout=30):
        h = {"User-Agent": UA}
        h.update(headers or {})
        req = urllib.request.Request(self.base + path, data=data, headers=h,
                                     method="POST" if data is not None else "GET")
        return urllib.request.urlopen(req, timeout=timeout)

    def get_text(self, path):
        with self._open(path) as r:
            return r.read().decode("utf-8", "replace")

    def post(self, thread, nick, message, extra=None):
        payload = {"message": message}
        if nick:
            payload["nick"] = nick
        hdrs = {"Content-Type": "application/json"}
        hdrs.update(extra or {})
        try:
            with self._open("/api/%s" % thread, json.dumps(payload).encode(), hdrs) as r:
                return r.status, r.headers, r.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.headers, e.read().decode("utf-8", "replace")

    def stream(self, thread, headers):
        h = {"Accept": "text/event-stream"}
        h.update(headers)
        return self._open("/api/%s/stream" % thread, headers=h, timeout=STREAM_TIMEOUT)


def parse_sse(resp):
    """Yield (event_id, data) per dispatched SSE event."""
    ev_id, data = None, []
    for raw in resp:
        line = raw.decode("utf-8", "replace").rstrip("\r\n")
        if not line:
            if data:
                yield ev_id, "\n".join(data)
            data = []
            continue
        if line.startswith(":"):
            continue
        field, _, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if field == "id":
            ev_id = value
        elif field == "data":
            data.append(value)


def follow(api, thread, last, history, stop, note, holder=None):
    """Yield (seq, obj) from the thread's SSE stream, reconnecting with backoff."""
    delay = 1
    while not stop.is_set():
        hdrs = {}
        if last is not None:
            hdrs["Last-Event-ID"] = str(last)
        elif history:
            hdrs["Last-Event-ID"] = "0"
        try:
            resp = api.stream(thread, hdrs)
            if holder is not None:
                holder["resp"] = resp
            with resp:
                delay = 1
                for ev_id, data in parse_sse(resp):
                    try:
                        obj = json.loads(data)
                    except ValueError:
                        obj = data
                    seq = to_int(obj.get("seq")) if isinstance(obj, dict) else None
                    if seq is None:
                        seq = to_int(ev_id)
                    if seq is not None:
                        if last is not None and seq <= last:
                            continue
                        last = seq
                    yield seq, obj
        except socket.timeout:
            continue
        except Exception as e:
            if stop.is_set():
                return
            note("stream: %s; retrying in %ds" % (e, delay))
            stop.wait(delay)
            delay = min(delay * 2, 60)
            continue
        stop.wait(1)  # server closed cleanly


# --------------------------------------------------------------------------- posting

def retry_after(headers, default=10):
    v = to_int(headers.get("Retry-After")) if headers else None
    return max(1, min(v, 60)) if v else default


def solve_pow(body):
    """Solve the server's Hashcash challenge from a 428 body.

    Body is {"token": "<epoch>:<nonce>:<sig>", "difficulty": N}. Find a counter such that
    sha256(token + counter) has >= N leading zero bits, and answer with the header
    X-PoW: <token>:<counter>. Tokens are single-use and expire after 60s.
    """
    try:
        ch = json.loads(body)
        token, bits = ch["token"], int(ch["difficulty"])
    except (ValueError, KeyError, TypeError):
        raise ValueError("unrecognised 428 challenge: %s" % clean(body.strip()[:80]))
    base = hashlib.sha256(token.encode())
    for n in range(1 << 26):
        h = base.copy()
        h.update(str(n).encode())
        if 256 - int.from_bytes(h.digest(), "big").bit_length() >= bits:
            return {"X-PoW": "%s:%d" % (token, n)}
    raise ValueError("gave up solving proof-of-work")


_BLANKS_RE = re.compile(r"\n{3,}")


def precheck(nick, msg):
    """Mirror the server's validation so obvious rejects fail locally. Returns error or None."""
    if len(nick.strip()) > 32 or not nick.isascii():
        return "nick (including #secret) must be at most 32 ASCII characters"
    s = _BLANKS_RE.sub("\n\n", clean(msg.replace("\r\n", "\n")).strip())
    if sum(1 for c in s if not c.isspace()) < 2:
        return "message needs at least 2 non-whitespace characters"
    if len(s.encode("utf-8")) > 1024:
        return "message is %d bytes, server limit is 1024" % len(s.encode("utf-8"))
    return None


def send(api, thread, nick, msg, log):
    err = precheck(nick, msg)
    if err:
        log("not sent: " + err)
        return False
    extra = None
    for _ in range(4):
        code, hdrs, body = api.post(thread, nick, msg, extra)
        if code < 300:
            return True
        if code == 429:
            wait = retry_after(hdrs)
            log("rate limited, retrying in %ds" % wait)
            time.sleep(wait)
            extra = None
            continue
        if code == 428:  # fresh single-use challenge each time, so just solve the new one
            try:
                extra = solve_pow(body)
                continue
            except ValueError as e:
                log("proof-of-work: %s" % e)
                return False
        log("post failed: HTTP %d %s" % (code, clean(body.strip()[:200])))
        return False
    log("giving up after repeated rate limiting / proof-of-work failures")
    return False


# --------------------------------------------------------------------------- ii mode

def make_fifo(path):
    try:
        st = os.stat(path)
    except FileNotFoundError:
        os.mkfifo(path, 0o600)
        return
    if not stat.S_ISFIFO(st.st_mode):
        raise SystemExit("%s exists and is not a FIFO" % path)


def read_fifo_lines(path):
    # O_RDWR keeps the FIFO open when writers come and go, so no EOF between `echo`s
    fd = os.open(path, os.O_RDWR)
    with os.fdopen(fd, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            yield line.rstrip("\r\n")


class Room:
    def __init__(self, client, name):
        self.c, self.name = client, name
        self.dir = os.path.join(client.root, name)
        os.makedirs(self.dir, mode=0o700, exist_ok=True)
        self.in_path = os.path.join(self.dir, "in")
        self.seq_path = os.path.join(self.dir, ".seq")
        make_fifo(self.in_path)
        self.out = open(os.path.join(self.dir, "out"), "a", encoding="utf-8", buffering=1)
        self.lock = threading.Lock()
        self.stop = threading.Event()
        self.holder = {}
        try:
            with open(self.seq_path) as f:
                self.last = to_int(f.read().strip())
        except OSError:
            self.last = None

    def write_lines(self, lines):
        with self.lock:
            if not self.out.closed:
                self.out.write("\n".join(lines) + "\n")

    def note(self, msg):
        self.write_lines(["%s -!- %s" % (now(), msg)])

    def start(self):
        threading.Thread(target=self._reader, daemon=True).start()
        threading.Thread(target=self._streamer, daemon=True).start()

    def close(self):
        self.stop.set()
        try:
            self.holder["resp"].close()
        except Exception:
            pass
        try:  # wake the reader so it notices stop
            fd = os.open(self.in_path, os.O_WRONLY | os.O_NONBLOCK)
            os.write(fd, b"\n")
            os.close(fd)
        except OSError:
            pass

    def _reader(self):
        for line in read_fifo_lines(self.in_path):
            if self.stop.is_set():
                break
            if line.strip():
                self._handle(line)

    def _handle(self, line):
        if line.startswith("//"):
            line = line[1:]
        elif line.startswith("/"):
            cmd, _, arg = line[1:].partition(" ")
            if cmd == "n":
                self.c.set_nick(arg.strip())
            elif cmd == "l":
                self.c.part(self.name)
            else:
                self.note("unknown command /%s (use //text for a literal slash)" % cmd)
            return
        send(self.c.api, self.name, self.c.nick, line.replace("\\n", "\n"), self.note)

    def _streamer(self):
        try:
            for seq, obj in follow(self.c.api, self.name, self.last, not self.c.no_history,
                                   self.stop, self.note, self.holder):
                self.write_lines(format_event(obj, seq))
                if seq is not None:
                    self.last = seq
                    try:
                        with open(self.seq_path, "w") as f:
                            f.write(str(seq))
                    except OSError:
                        pass
        finally:
            with self.lock:
                self.out.close()


class Client:
    def __init__(self, api, root, nick, no_history):
        self.api, self.root, self.nick, self.no_history = api, root, nick, no_history
        self.rooms = {}
        self.lock = threading.Lock()
        self.quit = threading.Event()
        os.makedirs(root, mode=0o700, exist_ok=True)
        self.in_path = os.path.join(root, "in")
        make_fifo(self.in_path)
        self.out = open(os.path.join(root, "out"), "a", encoding="utf-8", buffering=1)

    def log(self, msg):
        with self.lock:
            self.out.write("%s -!- %s\n" % (now(), msg))

    def set_nick(self, nick):
        self.nick = nick
        shown = nick.split("#", 1)[0] + ("#***" if "#" in nick else "")
        self.log("nick is now %s" % (shown or "(anonymous)"))

    def join(self, name):
        if not THREAD_RE.match(name):
            self.log("bad thread name %r (threads are single letters a-z)" % name)
            return
        if name in self.rooms:
            self.log("already in %s" % name)
            return
        room = Room(self, name)
        self.rooms[name] = room
        room.start()
        self.log("joined %s" % name)

    def part(self, name):
        room = self.rooms.pop(name, None)
        if room is None:
            self.log("not in %s" % name)
            return
        room.close()
        self.log("left %s" % name)

    def index(self):
        try:
            text = self.api.get_text("/api/index")
        except Exception as e:
            self.log("index: %s" % e)
            return
        for l in format_index(text):
            self.log("index: " + l)

    def _control(self):
        for line in read_fifo_lines(self.in_path):
            line = line.strip()
            if not line:
                continue
            cmd, _, arg = line.lstrip("/").partition(" ")
            if not line.startswith("/"):
                self.log("commands start with /: /j thread, /l thread, /i, /n nick#secret, /q")
            elif cmd == "j":
                for name in arg.split():
                    self.join(name)
            elif cmd == "l":
                for name in arg.split():
                    self.part(name)
            elif cmd == "i":
                self.index()
            elif cmd == "n":
                self.set_nick(arg.strip())
            elif cmd == "q":
                self.quit.set()
                return
            else:
                self.log("unknown command /%s" % cmd)

    def run(self, threads):
        self.log("connected to %s" % self.api.base)
        for t in threads:
            self.join(t)
        threading.Thread(target=self._control, daemon=True).start()
        while not self.quit.wait(1):
            pass
        for name in list(self.rooms):
            self.part(name)
        time.sleep(0.2)


# --------------------------------------------------------------------------- CLI

def normalize(url):
    if "://" not in url:
        url = "http://" + url
    return url.rstrip("/")


def cmd_tail(api, thread, new):
    stop = threading.Event()
    err = lambda m: print(m, file=sys.stderr)
    try:
        for seq, obj in follow(api, thread, None, not new, stop, err):
            for l in format_event(obj, seq):
                print(l, flush=True)
    except KeyboardInterrupt:
        pass


def main(argv=None):
    p = argparse.ArgumentParser(prog="webfartii", description="ii-style client for the webfart forum")
    p.add_argument("-s", "--server", default=os.environ.get("WEBFART_URL", "http://localhost:8080"),
                   help="server base URL (env WEBFART_URL; default %(default)s)")
    p.add_argument("-n", "--nick", default=os.environ.get("WEBFART_NICK", ""),
                   help="nick, optionally nick#secret for a tripcode (env WEBFART_NICK)")
    sub = p.add_subparsers(dest="cmd", required=True)

    ii = sub.add_parser("ii", help="run the FIFO/out-file daemon")
    ii.add_argument("-p", "--prefix", default=os.path.expanduser("~/webfart"),
                    help="directory tree root (default %(default)s)")
    ii.add_argument("--no-history", action="store_true",
                    help="on first join of a thread, don't ask the server to replay from seq 0")
    ii.add_argument("threads", nargs="*", help="threads to join at startup")

    c = sub.add_parser("cat", help="print a thread's plaintext view (/api/<t>.txt)")
    c.add_argument("thread")
    t = sub.add_parser("tail", help="follow a thread's live stream on stdout")
    t.add_argument("thread")
    t.add_argument("--new", action="store_true", help="skip history, only new posts")
    po = sub.add_parser("post", help="post one message (args, or stdin if none)")
    po.add_argument("thread")
    po.add_argument("message", nargs="*")
    sub.add_parser("index", help="list active threads")

    a = p.parse_args(argv)
    base = normalize(a.server)
    api = Api(base)

    if a.cmd != "ii" and hasattr(a, "thread") and not THREAD_RE.match(a.thread):
        p.error("bad thread name (threads are single letters a-z)")

    if a.cmd == "ii":
        if not hasattr(os, "mkfifo"):
            p.error("ii mode needs a Unix with mkfifo; use cat/tail/post instead")
        host = re.sub(r"[^A-Za-z0-9._-]", "_", urllib.parse.urlparse(base).netloc) or "server"
        client = Client(api, os.path.join(a.prefix, host), a.nick, a.no_history)
        signal.signal(signal.SIGTERM, lambda *_: client.quit.set())
        print("webfartii: %s" % client.root, file=sys.stderr)
        try:
            client.run(a.threads)
        except KeyboardInterrupt:
            pass
        return 0
    if a.cmd == "cat":
        try:
            sys.stdout.write(clean(api.get_text("/api/%s.txt" % a.thread)))
        except urllib.error.URLError as e:
            print("cat: %s" % e, file=sys.stderr)
            return 1
        return 0
    if a.cmd == "tail":
        cmd_tail(api, a.thread, a.new)
        return 0
    if a.cmd == "post":
        msg = " ".join(a.message) if a.message else sys.stdin.read().rstrip("\n")
        if not msg.strip():
            p.error("empty message")
        ok = send(api, a.thread, a.nick, msg, lambda m: print("post: " + m, file=sys.stderr))
        return 0 if ok else 1
    if a.cmd == "index":
        try:
            print("\n".join(format_index(api.get_text("/api/index"))))
        except urllib.error.URLError as e:
            print("index: %s" % e, file=sys.stderr)
            return 1
        return 0


if __name__ == "__main__":
    sys.exit(main())
