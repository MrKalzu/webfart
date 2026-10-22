(function() {
  "use strict";

  const HOME_ROW = ["a", "s", "d", "f", "g", "h", "j", "k", "l"];
  let curThread = "h", latestSeq = 0, eventSource = null, pollInterval = null;
  const seenSeqs = new Set();
  
  const BASE_TITLE = "Webfart Forum";
  let unreadReplies = 0;
  let isInitialLoad = true;
  let mySeqs = new Set();

  function loadMySeqs() {
    try {
      const stored = JSON.parse(sessionStorage.getItem("wf_my_seqs_" + curThread) || "[]");
      mySeqs = new Set(stored);
    } catch (e) {
      mySeqs = new Set();
    }
  }

  function recordMySeq(seq) {
    mySeqs.add(seq);
    try {
      sessionStorage.setItem("wf_my_seqs_" + curThread, JSON.stringify(Array.from(mySeqs)));
    } catch (e) {}
  }

  function updateTitle() {
    if (unreadReplies > 0) {
      document.title = "(" + unreadReplies + ") /" + curThread + "/ - " + BASE_TITLE;
    } else {
      document.title = "/" + curThread + "/ - " + BASE_TITLE;
    }
  }

  function quotesMe(msg) {
    if (!msg.body || mySeqs.size === 0) return false;
    // Don't notify if you quoted yourself in your own post
    if (mySeqs.has(msg.seq)) return false;

    const quoteRegex = />>(\d+)/g;
    let match;
    while ((match = quoteRegex.exec(msg.body)) !== null) {
      if (mySeqs.has(parseInt(match[1], 10))) {
        return true;
      }
    }
    return false;
  }

  const viewport = document.getElementById("messages-viewport");
  const messagesList = document.getElementById("messages-list");
  const unreadPill = document.getElementById("unread-pill");
  const postBar = document.getElementById("post-bar");
  const nickInput = document.getElementById("nick-input");
  const msgInput = document.getElementById("msg-input");
  const sendBtn = document.getElementById("send-btn");
  const threadsNav = document.getElementById("threads-nav");
  const curLabel = document.getElementById("current-thread-label");
  const anchorInfo = document.getElementById("anchor-info");
  const streamStatus = document.getElementById("stream-status");
  const byteCounter = document.getElementById("byte-counter");
  const enc = new TextEncoder();
  
  // Touch keyboards have no Shift+Enter, so there Enter must insert a newline
  // (needed for multi-line code blocks) and the Send button does the sending.
  const coarsePointer = window.matchMedia && window.matchMedia("(pointer: coarse)").matches;
  if (!coarsePointer) msgInput.placeholder = "Write message... (Enter to send)";

  function updateCounter() {
    const bytes = enc.encode(msgInput.value).length;
    byteCounter.textContent = bytes + "/1024 B";
    if (bytes > 1024) {
      byteCounter.style.color = "#e06c75"; // Red
      sendBtn.disabled = true;
    } else if (bytes > 900) {
      byteCounter.style.color = "#d19a66"; // Amber warning
      sendBtn.disabled = false;
    } else {
      byteCounter.style.color = "var(--text-muted)";
      sendBtn.disabled = false;
    }
  }
  msgInput.addEventListener("input", updateCounter);

  function startCooldown(seconds) {
    let remaining = seconds;
    sendBtn.disabled = true;
    sendBtn.textContent = remaining + "s";
    const timer = setInterval(function() {
      remaining--;
      if (remaining <= 0) {
        clearInterval(timer);
        sendBtn.disabled = false;
        sendBtn.textContent = "Send";
      } else {
        sendBtn.textContent = remaining + "s";
      }
    }, 1000);
  }


  function getThreadFromPath() {
    const p = window.location.pathname.replace(/^\/|\/$/g, "");
    return /^[a-z]$/.test(p) ? p : "h";
  }

  function applyTint(el, pid) {
    if (!pid) return;
    let hash = 0;
    for (let i = 0; i < pid.length; i++) {
      hash = pid.charCodeAt(i) + ((hash << 5) - hash);
    }
    el.style.backgroundColor = "hsl(" + Math.abs(hash % 360) + ", 20%, 13%)";
  }

  function isNearBottom() {
    return viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight <= 50;
  }

  function scrollToBottom() {
    viewport.scrollTop = viewport.scrollHeight;
    unreadPill.style.display = "none";
  }

  unreadPill.addEventListener("click", scrollToBottom);
  viewport.addEventListener("scroll", function() {
    if (isNearBottom()) unreadPill.style.display = "none";
  });

  function dom(tag, cls, text) {
    const d = document.createElement(tag);
    if (cls) d.className = cls;
    if (text !== undefined) d.textContent = text;
    return d;
  }

  function highlightJSON(jsonStr, container) {
    // Matches: 1: strings (with optional trailing colon for keys), 2: booleans/null, 3: numbers
    const jsonRegex = /("(?:\\u[\da-fA-F]{4}|\\[^u]|[^\\"])*")(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?/g;
    let lastIdx = 0, match;

    while ((match = jsonRegex.exec(jsonStr)) !== null) {
      if (match.index > lastIdx) {
        container.appendChild(document.createTextNode(jsonStr.substring(lastIdx, match.index)));
      }

      if (match[1]) {
        if (match[2]) {
          // Key name
          container.appendChild(dom("span", "json-key", match[1]));
          container.appendChild(document.createTextNode(match[2]));
        } else {
          // String value
          container.appendChild(dom("span", "json-string", match[1]));
        }
      } else if (match[3]) {
        // Boolean or null
        const cls = match[3] === "null" ? "json-null" : "json-boolean";
        container.appendChild(dom("span", cls, match[3]));
      } else {
        // Number
        container.appendChild(dom("span", "json-number", match[0]));
      }

      lastIdx = jsonRegex.lastIndex;
    }

    if (lastIdx < jsonStr.length) {
      container.appendChild(document.createTextNode(jsonStr.substring(lastIdx)));
    }
  }

  // ---------------------------------------------------------------------
  // JSON pretty-printing
  //
  // JSON.stringify(JSON.parse(s), null, 2) is the obvious way, but it
  // rewrites data: integers beyond 2^53 lose digits, 1.0 becomes 1, and
  // integer-like keys ("2" before "1") get reordered. Instead we validate
  // with JSON.parse (strict, native) and then only re-indent: every
  // string/number/literal token is copied through verbatim.
  // ---------------------------------------------------------------------
  const PRETTY_WIDTH = 72;

  function prettyJSON(src) {
    const t = src.match(/"(?:\\.|[^"\\])*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null|[{}\[\],:]/g);
    let i = 0;

    function parse() {
      const x = t[i++];
      if (x === "{") {
        const items = [];
        if (t[i] === "}") { i++; return { o: items }; }
        for (;;) {
          const k = t[i++]; i++; // key, then ':'
          items.push([k, parse()]);
          if (t[i++] === "}") return { o: items };
        }
      }
      if (x === "[") {
        const items = [];
        if (t[i] === "]") { i++; return { a: items }; }
        for (;;) {
          items.push(parse());
          if (t[i++] === "]") return { a: items };
        }
      }
      return { v: x };
    }

    function flat(n) {
      if (n.v !== undefined) return n.v;
      if (n.a) return n.a.length ? "[" + n.a.map(flat).join(", ") + "]" : "[]";
      return n.o.length ? "{" + n.o.map(function(p) { return p[0] + ": " + flat(p[1]); }).join(", ") + "}" : "{}";
    }

    function out(n, depth) {
      if (n.v !== undefined) return n.v;
      const empty = n.a ? !n.a.length : !n.o.length;
      // Empty containers and short scalar-only arrays stay on one line
      if (empty || (n.a && n.a.every(function(x) { return x.v !== undefined; }) &&
                    flat(n).length + depth * 2 <= PRETTY_WIDTH)) {
        return flat(n);
      }
      const inner = "  ".repeat(depth + 1), outer = "  ".repeat(depth);
      if (n.a) {
        return "[\n" + n.a.map(function(x) { return inner + out(x, depth + 1); }).join(",\n") + "\n" + outer + "]";
      }
      return "{\n" + n.o.map(function(p) { return inner + p[0] + ": " + out(p[1], depth + 1); }).join(",\n") + "\n" + outer + "}";
    }

    return out(parse(), 0);
  }

  // Returns display text if `s` is a JSON object/array, else null.
  // Compact (single-line) JSON is expanded; JSON you already formatted
  // across several lines is left exactly as written.
  function tryJSON(s) {
    const t = s.trim();
    if (!/^[\[{]/.test(t)) return null;
    try { JSON.parse(t); } catch (e) { return null; }
    if (t.indexOf("\n") !== -1) return t;
    try { return prettyJSON(t); } catch (e) { return t; }
  }

  // ---------------------------------------------------------------------
  // Table-driven syntax highlighting
  //
  // Each language is a small def: comment / string / extra regex sources,
  // plus word lists. compile() joins them into ONE regex, one capture group
  // per source (so sources must use (?:...) only), and highlightCode() walks
  // the matches. Output is built with textContent only, never innerHTML.
  // ---------------------------------------------------------------------
  const EOF = "(?![\\s\\S])";
  const DQ  = String.raw`"(?:\\.|[^"\\\n])*"?`;          // "..." one line, tolerates a missing close
  const SQ  = String.raw`'(?:\\.|[^'\\\n])*'?`;
  const CH  = String.raw`'(?:\\.[^'\n]*|[^'\\\n])'`;      // char literal, so Rust lifetimes etc. survive
  const BT  = "`[^`]*`?";                                   // backtick string, may span lines
  const BC  = String.raw`/\*[\s\S]*?(?:\*/|` + EOF + ")";  // /* block comment */
  const NUM = String.raw`\b(?:0[xX][\da-fA-F_]+|\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?)\b`;
  const HASH = String.raw`(?<![^\s])#.*`;                   // # comment at line start or after whitespace

  const LANGS = new Map();
  function lang(names, def) {
    names.split(" ").forEach(function(n) { LANGS.set(n, def); });
  }

  lang("go golang", {
    cm: ["//.*", BC], str: [DQ, BT, CH],
    kw: "break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var",
    lit: "true false nil iota",
    ty: "string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr float32 float64 bool byte rune error any"
  });

  lang("py python", {
    cm: ["#.*"],
    str: [String.raw`"""[\s\S]*?(?:"""|` + EOF + String.raw`)|'''[\s\S]*?(?:'''|` + EOF + ")", DQ, SQ],
    x: [["t", String.raw`@\w+(?:\.\w+)*`]],
    kw: "and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case",
    lit: "True False None"
  });

  lang("js javascript ts typescript jsx tsx mjs", {
    cm: ["//.*", BC], str: [DQ, SQ, BT],
    kw: "async await break case catch class const continue debugger default delete do else export extends finally for from function if import in instanceof let new of return static super switch this throw try typeof var void while with yield interface type enum implements readonly as satisfies",
    lit: "true false null undefined NaN Infinity",
    ty: "string number boolean any unknown never object"
  });

  lang("sh bash shell zsh", {
    cm: [HASH],
    str: [String.raw`"(?:\\[\s\S]|[^"\\])*"?`, String.raw`'[^']*'?`],
    x: [["v", String.raw`\$(?:\w+|\{[^}\n]*\}|[@#?$!*-])`],
        ["l", String.raw`(?<![\w-])--?[A-Za-z][\w-]*`]],   // command-line options
    kw: "if then else elif fi for while until do done case esac in function select return exit export local readonly declare unset source alias set shift trap break continue",
    lit: "true false"
  });

  lang("sql postgres postgresql psql sqlite mysql", {
    ci: true,
    cm: ["--.*", BC],
    str: [String.raw`'(?:''|[^'])*'?`, String.raw`"[^"\n]*"?`],
    kw: "select from where and or not in is as on join left right inner outer full cross group by order having limit offset insert into values update set delete create alter drop table index view database schema primary key foreign references unique default check constraint distinct union all intersect except case when then else end exists between like ilike asc desc with returning begin commit rollback transaction explain analyze if cascade add column",
    lit: "null true false",
    ty: "int integer bigint smallint text varchar char boolean bool date time timestamp timestamptz serial uuid json jsonb numeric decimal float real double blob"
  });

  lang("yaml yml", {
    cm: [HASH], str: [DQ, SQ],
    x: [["key", String.raw`(?<![^\s])[\w.\-/]+(?=[ \t]*:(?:\s|` + EOF + "))"],
        ["v", String.raw`[&*][\w-]+`]],                      // anchors and aliases
    lit: "true false null yes no on off True False"
  });

  lang("toml ini conf", {
    cm: [String.raw`(?<![^\s])[#;].*`], str: [DQ, SQ],
    x: [["t", String.raw`^[ \t]*\[\[?[^\]\n]*\]\]?`],       // [section] / [[table]]
        ["key", String.raw`^[ \t]*[\w.\-]+(?=[ \t]*=)`]],
    lit: "true false"
  });

  lang("nginx", {
    cm: [HASH], str: [DQ, SQ],
    x: [["v", String.raw`\$(?:\w+|\{\w+\})`],
        // directive = first word on a line or right after ; or {
        ["k", String.raw`(?<=^[ \t]*|[;{][ \t]*)[a-z_]\w*`]],
    lit: "on off"
  });

  // Caddyfile: intentionally lightweight highlighting for common directives.
  lang("caddy caddyfile", {
    cm: [HASH], str: [DQ, SQ, BT],
    x: [["v", String.raw`\{[\w.$-]+\}`],
        ["l", String.raw`@[A-Za-z0-9_-]+`],
        ["k", String.raw`^[ \t]*[a-z_][\w-]*(?=[ \t]|\{|$)`]],
    lit: "true false on off"
  });

  lang("dockerfile docker", {
    cm: [HASH], str: [DQ, SQ],
    x: [["k", String.raw`^[ \t]*(?:ADD|ARG|CMD|COPY|ENTRYPOINT|ENV|EXPOSE|FROM|HEALTHCHECK|LABEL|MAINTAINER|ONBUILD|RUN|SHELL|STOPSIGNAL|USER|VOLUME|WORKDIR)(?!\w)`],
        ["k", String.raw`(?<=\s)AS(?=\s)`],
        ["v", String.raw`\$(?:\w+|\{[^}\n]*\})`],
        ["l", String.raw`(?<![\w-])--[a-z][\w-]*`]]
  });

  lang("html htm xml svg", {
    cm: [String.raw`<!--[\s\S]*?(?:-->|` + EOF + ")"],
    str: [String.raw`(?<==)(?:"[^"\n]*"?|'[^'\n]*'?)`],      // attribute values only, so apostrophes in text are safe
    x: [["t", String.raw`</?[A-Za-z][\w:.-]*`],
        ["v", String.raw`(?<=[\s"'])[A-Za-z_:@][\w:.@-]*(?=\s*=)`],
        ["l", String.raw`&#?\w+;`]],
    num: false
  });

  lang("diff patch", {
    lines: [[/^(?:\+\+\+|---|diff |index )/, "meta"], [/^@@/, "hunk"], [/^\+/, "add"], [/^-/, "del"]]
  });

  function compileLang(d) {
    const parts = [], cls = [];
    function add(c, src) { cls.push(c); parts.push("(" + src + ")"); }
    (d.cm || []).forEach(function(s) { add("c", s); });
    (d.str || []).forEach(function(s) { add("s", s); });
    (d.x || []).forEach(function(p) { add(p[0], p[1]); });
    const words = new Map();
    [["k", d.kw], ["l", d.lit], ["t", d.ty]].forEach(function(p) {
      (p[1] || "").split(" ").forEach(function(w) {
        if (w) words.set(d.ci ? w.toLowerCase() : w, p[0]);
      });
    });
    if (words.size) add("w", String.raw`[A-Za-z_]\w*`);   // identifiers, classified by word list
    if (d.num !== false) add("n", NUM);
    d.cls = cls;
    d.words = words;
    d.re = new RegExp(parts.join("|"), "gm");
  }

  function highlightCode(code, container, d) {
    if (d.lines) {
      code.split("\n").forEach(function(line, idx) {
        if (idx > 0) container.appendChild(document.createTextNode("\n"));
        const rule = d.lines.find(function(r) { return r[0].test(line); });
        container.appendChild(rule ? dom("span", "hl-" + rule[1], line) : document.createTextNode(line));
      });
      return;
    }
    if (!d.re) compileLang(d);
    const re = d.re;
    re.lastIndex = 0;
    let last = 0, m;
    while ((m = re.exec(code)) !== null) {
      if (m[0] === "") { re.lastIndex++; continue; }
      let gi = 1;
      while (m[gi] === undefined) gi++;
      let c = d.cls[gi - 1];
      if (c === "w") c = d.words.get(d.ci ? m[0].toLowerCase() : m[0]);
      if (m.index > last) container.appendChild(document.createTextNode(code.substring(last, m.index)));
      container.appendChild(c ? dom("span", "hl-" + c, m[0]) : document.createTextNode(m[0]));
      last = re.lastIndex;
    }
    if (last < code.length) container.appendChild(document.createTextNode(code.substring(last)));
  }

  // One entry point for every code block (fenced or bare JSON)
  function appendCodeBlock(container, raw, langTag) {
    const pre = dom("pre", "md-code-block");
    const code = dom("code", null);
    const json = (!langTag || langTag === "json") ? tryJSON(raw) : null;
    const def = LANGS.get(langTag);
    if (json !== null) highlightJSON(json, code);
    else if (def) highlightCode(raw, code, def);
    else if (langTag === "json") highlightJSON(raw, code);   // invalid/JSONL: still colour tokens
    else code.textContent = raw;
    pre.appendChild(code);
    container.appendChild(pre);
  }

  function parseInline(text, container) {
    // Matches: 1: >>seq | 2: URLs | 3: `inline code` | 4: **bold**
    const tokenRegex = /(>>\d+)|(https?:\/\/[^\s<]+[^<.,:;"')\]\s])|(`[^`\n]+`)|(\*\*[^*\n]+\*\*)/g;
    let lastIdx = 0, match;

    while ((match = tokenRegex.exec(text)) !== null) {
      if (match.index > lastIdx) {
        container.appendChild(document.createTextNode(text.substring(lastIdx, match.index)));
      }

      if (match[1]) {
        // >>123 quote link
        const qLink = dom("a", "quote-link", match[1]);
        qLink.href = "#m" + match[1].substring(2);
        container.appendChild(qLink);
      } else if (match[2]) {
        // http(s):// clickable link
        const url = match[2];
        const a = dom("a", "url-link", url);
        a.href = url;
        a.target = "_blank";
        a.rel = "noopener noreferrer"; // Security against reverse tabnabbing
        container.appendChild(a);
      } else if (match[3]) {
        // `inline code`
        container.appendChild(dom("code", "inline-code", match[3].slice(1, -1)));
      } else if (match[4]) {
        // **bold**
        container.appendChild(dom("strong", "md-bold", match[4].slice(2, -2)));
      }

      lastIdx = tokenRegex.lastIndex;
    }

    if (lastIdx < text.length) {
      container.appendChild(document.createTextNode(text.substring(lastIdx)));
    }
  }

  // Detect bare Nginx/Caddy configs only when the whole message is small.
  // The 1 KiB limit keeps ordinary prose and large messages out of this heuristic.
  function detectBareConfig(text) {
    const s = text.trim();
    if (!s || s.length > 1024) return null;

    // Nginx needs both a recognizable block and at least one directive.
    const nginxBlock = /^\s*(?:server|http|events|upstream|stream)\s*\{/m.test(s);
    const nginxDirective = /^\s*(?:listen|server_name|location|proxy_pass|root|try_files|worker_processes|proxy_set_header)\b/m.test(s);
    if (nginxBlock && nginxDirective) return "nginx";

    // Caddy: a site/address block plus a distinctive Caddy directive.
    const caddyBlock = /^\s*(?:[\w*.-]+(?::\d+)?|\[[^\]]+\]|:\d+)\s*\{/m.test(s);
    const caddyDirective = /^\s*(?:reverse_proxy|file_server|php_fastcgi|handle|handle_path|redir|encode|tls|try_files)\b/m.test(s);
    if (caddyBlock && caddyDirective) return "caddy";

    return null;
  }

  function renderMicroMarkdown(text, container) {
    // Auto-highlight small, bare Nginx/Caddy configuration messages.
    const configLang = detectBareConfig(text);
    if (configLang) {
      appendCodeBlock(container, text.trim(), configLang);
      return;
    }

    // A message that is nothing but a JSON object/array renders as a code block
    if (/^\s*[\[{]/.test(text) && tryJSON(text) !== null) {
      appendCodeBlock(container, text, "json");
      return;
    }

    const lines = text.split("\n");
    let inCodeBlock = false;
    let codeBuffer = [];
    let codeLang = ""; // <-- Track language

    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];

      // Code block toggle: ``` or ```json
      if (line.trim().startsWith("```")) {
        if (inCodeBlock) {
          // Closing code block
          appendCodeBlock(container, codeBuffer.join("\n"), codeLang);
          codeBuffer = [];
          codeLang = "";
          inCodeBlock = false;
        } else {
          // Opening code block: read tag like ```json
          inCodeBlock = true;
          codeLang = line.trim().slice(3).trim().toLowerCase().split(/\s+/)[0];
        }
        continue;
      }

      if (inCodeBlock) {
        codeBuffer.push(line);
        continue;
      }

      // Headers: #, ##, ###
      const headerMatch = line.match(/^(#{1,3})\s+(.+)$/);
      if (headerMatch) {
        const level = headerMatch[1].length;
        const headerEl = dom("div", "md-header md-h" + level);
        parseInline(headerMatch[2], headerEl);
        container.appendChild(headerEl);
        continue;
      }

      // Standard line
      const lineDiv = dom("div", "md-line");
      if (line.length > 0) {
        parseInline(line, lineDiv);
      }
      container.appendChild(lineDiv);
    }

    // Flush any unclosed code blocks
    if (inCodeBlock && codeBuffer.length > 0) {
      appendCodeBlock(container, codeBuffer.join("\n"), codeLang);
    }
  }


  function createMessageElement(msg) {
    const card = dom("div", "message-card");
    card.id = "m" + msg.seq;
    if (msg.pid) applyTint(card, msg.pid);

    const header = dom("div", "message-header");
    const d = new Date(msg.created_at);
    header.appendChild(dom("span", null, "[" + d.toTimeString().split(" ")[0] + "]"));
    header.appendChild(dom("span", "author-pid", "<" + (msg.pid || "deleted") + ">"));

    if (msg.nick && msg.nick !== msg.pid) {
      header.appendChild(dom("span", "author-nick", msg.nick));
    }

    const seqLink = dom("a", "seq-link", "(seq: " + msg.seq + ")");
    seqLink.href = "#m" + msg.seq;
    seqLink.addEventListener("click", function() {
      if (!msgInput.value.includes(">>" + msg.seq)) {
        msgInput.value = (msgInput.value ? msgInput.value + " " : "") + ">>" + msg.seq + " ";
        msgInput.focus();
      }
    });
    header.appendChild(seqLink);
    card.appendChild(header);

    const bodyDiv = dom("div", "message-body");
    if (msg.body === null || msg.body === undefined) {
      bodyDiv.appendChild(dom("span", "deleted-text", "[message tombstoned]"));
    } else {
      renderMicroMarkdown(msg.body, bodyDiv);
    }
    card.appendChild(bodyDiv);
    return card;
  }

  function appendMessage(msg) {
    if (seenSeqs.has(msg.seq)) {
      const existing = document.getElementById("m" + msg.seq);
      if (existing && msg.body === null) {
        existing.replaceWith(createMessageElement(msg));
      }
      return;
    }
    seenSeqs.add(msg.seq);
    if (msg.seq > latestSeq) latestSeq = msg.seq;

    const wasNear = isNearBottom();
    messagesList.appendChild(createMessageElement(msg));
    if (wasNear) {
      scrollToBottom();
    } else {
      unreadPill.style.display = "block";
    }
  }

  function loadInitialMessages() {
    fetch("/api/" + curThread + "?limit=100")
      .then(function(r) { return r.json(); })
      .then(function(d) {
        if (d.anchor) anchorInfo.textContent = "Anchor: " + d.anchor.substring(0, 16) + "...";
        if (Array.isArray(d.messages)) {
          d.messages.forEach(appendMessage);
          scrollToBottom();
        }
      })
      .catch(function() {});
  }

  function connectSSE() {
    if (eventSource) eventSource.close();
    streamStatus.textContent = "Connecting stream...";
    try {
      eventSource = new EventSource("/api/" + curThread + "/stream?since_seq=" + latestSeq);
      eventSource.addEventListener("message", function(e) {
        streamStatus.textContent = "Live SSE";
        try { appendMessage(JSON.parse(e.data)); } catch (err) {}
      });
      eventSource.onerror = function() {
        streamStatus.textContent = "Reconnecting...";
        if (eventSource.readyState === EventSource.CLOSED) fallbackPolling();
      };
      eventSource.onopen = function() {
        streamStatus.textContent = "Live SSE";
        if (pollInterval) { clearInterval(pollInterval); pollInterval = null; }
      };
    } catch (e) {
      fallbackPolling();
    }
  }

  function fallbackPolling() {
    if (pollInterval) return;
    streamStatus.textContent = "Polling fallback";
    pollInterval = setInterval(function() {
      fetch("/api/" + curThread + "?since_seq=" + latestSeq)
        .then(function(r) { return r.json(); })
        .then(function(d) {
          if (Array.isArray(d.messages)) d.messages.forEach(appendMessage);
        }).catch(function() {});
    }, 3000);
  }

  document.addEventListener("visibilitychange", function() {
    if (document.visibilityState === "hidden") {
      if (eventSource) { eventSource.close(); eventSource = null; }
      if (pollInterval) { clearInterval(pollInterval); pollInterval = null; }
      streamStatus.textContent = "Paused";
    } else {
      connectSSE();
    }
  });

  async function solvePoW(token, difficulty) {
    streamStatus.textContent = "Solving PoW (" + difficulty + " bits)...";
    const enc = new TextEncoder();
    const full = Math.floor(difficulty / 8);
    const rem = difficulty % 8;
    const mask = rem > 0 ? (0xFF << (8 - rem)) & 0xFF : 0;
    let ctr = 0;

    while (true) {
      const buf = await crypto.subtle.digest("SHA-256", enc.encode(token + ctr));
      const h = new Uint8Array(buf);
      let ok = true;
      for (let i = 0; i < full; i++) {
        if (h[i] !== 0) { ok = false; break; }
      }
      if (ok && rem > 0 && (h[full] & mask) !== 0) ok = false;
      if (ok) {
        streamStatus.textContent = "Live SSE";
        return "" + ctr;
      }
      ctr++;
      if (ctr % 4000 === 0) await new Promise(function(r) { setTimeout(r, 0); });
    }
  }
  // Handle Enter to send, Shift+Enter for new line
  msgInput.addEventListener("keydown", function(e) {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !coarsePointer) {
      e.preventDefault();
      if (typeof postBar.requestSubmit === "function") {
        postBar.requestSubmit();
      } else {
        sendBtn.click();
      }
    }
  });

  postBar.addEventListener("submit", async function(e) {
    e.preventDefault();
    const raw = msgInput.value.trim();
    if (!raw) return;

    const payload = { nick: nickInput.value.trim(), message: raw };
    sendBtn.disabled = true;

    async function sendPost(xpow) {
      const headers = { "Content-Type": "application/json" };
      if (xpow) headers["X-PoW"] = xpow;
      const res = await fetch("/api/" + curThread, {
        method: "POST",
        headers: headers,
        body: JSON.stringify(payload)
      });
      if (res.status === 201) {
        startCooldown(1);
        appendMessage(await res.json());
        msgInput.value = "";
        updateCounter();
        return true;
      }
      if (res.status === 428) {
        const ch = await res.json();
        const ctr = await solvePoW(ch.token, ch.difficulty);
        return await sendPost(ch.token + ":" + ctr);
      }
      if (res.status === 429) {
        alert("Rate limit active: please wait " + (res.headers.get("Retry-After") || 10) + "s.");
        return false;
      }
      alert("Post failed (" + res.status + "): " + (await res.text()));
      return false;
    }

    try {
      await sendPost(null);
    } catch (err) {
      alert("Error: " + err);
    } finally {
      sendBtn.disabled = false;
      msgInput.focus();
    }
  });

  function renderNav(active) {
    while (threadsNav.firstChild) threadsNav.removeChild(threadsNav.firstChild);
    const set = new Set(HOME_ROW);
    if (Array.isArray(active)) active.forEach(function(t) { set.add(t); });
    Array.from(set).sort().forEach(function(t) {
      const a = dom("a", "nav-pill" + (t === curThread ? " active" : ""), "[" + t + "]");
      a.href = "/" + t;
      a.addEventListener("click", function(e) {
        e.preventDefault();
        if (t !== curThread) {
          curThread = t;
          window.history.pushState(null, "", "/" + t + (nickInput.value ? "#" + nickInput.value : ""));
          initThread();
        }
      });
      threadsNav.appendChild(a);
    });
  }

  function initThread() {
    curLabel.textContent = "/" + curThread + "/";
    latestSeq = 0;
    seenSeqs.clear();
    while (messagesList.firstChild) messagesList.removeChild(messagesList.firstChild);

    fetch("/api/index")
      .then(function(r) { return r.json(); })
      .then(function(d) { renderNav(d.active_threads); })
      .catch(function() { renderNav(HOME_ROW); });

    loadInitialMessages();
    connectSSE();
  }

  window.addEventListener("popstate", function() {
    const t = getThreadFromPath();
    if (t !== curThread) {
      curThread = t;
      initThread();
    }
  });

  curThread = getThreadFromPath();
  if (window.location.hash) {
    const h = window.location.hash.substring(1).trim();
    if (h && !h.startsWith("m")) nickInput.value = h;
  }
  initThread();
})();
