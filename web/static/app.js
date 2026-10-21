(function() {
  "use strict";

  const HOME_ROW = ["a", "s", "d", "f", "g", "h", "j", "k", "l"];
  let curThread = "h", latestSeq = 0, eventSource = null, pollInterval = null;
  const seenSeqs = new Set();

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

  function renderMicroMarkdown(text, container) {

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
          const pre = dom("pre", "md-code-block");
          const code = dom("code", null);
          const rawCode = codeBuffer.join("\n");

          // If marked as ```json (or if it looks like JSON)
          if (codeLang === "json" || (rawCode.trim().startsWith("{") && rawCode.trim().endsWith("}"))) {
            highlightJSON(rawCode, code);
          } else {
            code.textContent = rawCode;
          }

          pre.appendChild(code);
          container.appendChild(pre);
          codeBuffer = [];
          codeLang = "";
          inCodeBlock = false;
        } else {
          // Opening code block: read tag like ```json
          inCodeBlock = true;
          codeLang = line.trim().slice(3).trim().toLowerCase();
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
      const pre = dom("pre", "md-code-block");
      pre.appendChild(dom("code", null, codeBuffer.join("\n")));
      container.appendChild(pre);
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
    if (e.key === "Enter" && !e.shiftKey) {
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
        appendMessage(await res.json());
        msgInput.value = "";
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
