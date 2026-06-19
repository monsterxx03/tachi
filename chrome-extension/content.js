// content.js — Tachi Chrome Extension Content Script
//
// Injected into every page. Handles floating toast UI for displaying
// results from Tachi. Communicates with background.js via chrome.runtime.

// ── Message Listener ─────────────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  switch (msg.type) {
    case "show_loading":
      showLoading(msg.action);
      break;
    case "show_result":
      hideLoading();
      showToast(msg.action, msg.content);
      break;
    case "show_error":
      hideLoading();
      showToast("error", msg.content, true);
      break;
  }
});

// ── Toast Icons ──────────────────────────────────────────────────────────────

const ACTION_ICONS = {
  "ask_tachi": "🤖",
  "explain":   "📖",
  "search":    "🔍",
  "remember":  "🧠",
  "recall":    "🔎",
  "error":     "⚠️",
  "loading":   "⏳",
};

const ACTION_TITLES = {
  "ask_tachi": "问 Tachi",
  "explain":   "解释这个概念",
  "search":    "搜索",
  "remember":  "存入记忆",
  "recall":    "搜索记忆",
  "error":     "出错了",
  "loading":   "思考中…",
};

// ── Loading Indicator ────────────────────────────────────────────────────────

function showLoading(action) {
  const existing = document.getElementById("tachi-toast");
  if (existing) existing.remove();

  const toast = document.createElement("div");
  toast.id = "tachi-toast";
  toast.className = "tachi-toast tachi-loading";
  toast.innerHTML = `
    <div class="tachi-header">
      <span class="tachi-icon">${ACTION_ICONS[action] || "🤖"}</span>
      <span class="tachi-title">${ACTION_TITLES[action] || "Tachi"}</span>
      <div class="tachi-spinner"></div>
    </div>
  `;

  document.body.appendChild(toast);

  // Auto-hide loading after 30s (timeout guard)
  toast._loadingTimer = setTimeout(() => {
    const stillLoading = document.getElementById("tachi-toast");
    if (stillLoading) {
      stillLoading.remove();
    }
  }, 30000);
}

function hideLoading() {
  const toast = document.getElementById("tachi-toast");
  if (toast && toast._loadingTimer) {
    clearTimeout(toast._loadingTimer);
  }
  // Don't remove immediately — the result will replace it via showToast
}

// ── Toast Display ────────────────────────────────────────────────────────────

function showToast(action, content, isError = false) {
  // Remove existing toast
  const existing = document.getElementById("tachi-toast");
  if (existing) existing.remove();

  const toast = document.createElement("div");
  toast.id = "tachi-toast";
  toast.className = "tachi-toast";

  // Sanitize content: render markdown-like formatting safely
  const rendered = renderContent(content);

  toast.innerHTML = `
    <div class="tachi-header">
      <span class="tachi-icon">${ACTION_ICONS[action] || "🤖"}</span>
      <span class="tachi-title">${ACTION_TITLES[action] || "Tachi"}</span>
      <span class="tachi-actions">
        <button class="tachi-btn tachi-btn-copy" title="复制">📋</button>
        <button class="tachi-btn tachi-btn-pin" title="固定">📌</button>
        <button class="tachi-btn tachi-btn-close">×</button>
      </span>
    </div>
    <div class="tachi-body">${rendered}</div>
  `;

  document.body.appendChild(toast);

  // ── Event Handlers ──

  // Close button
  toast.querySelector(".tachi-btn-close").onclick = () => toast.remove();

  // Copy button
  toast.querySelector(".tachi-btn-copy").onclick = () => {
    // Copy plain text (strip HTML)
    const text = toast.querySelector(".tachi-body").textContent || content;
    navigator.clipboard.writeText(text).then(() => {
      const btn = toast.querySelector(".tachi-btn-copy");
      btn.textContent = "✅";
      setTimeout(() => { btn.textContent = "📋"; }, 1500);
    });
  };

  // Pin toggle
  toast.querySelector(".tachi-btn-pin").onclick = function() {
    toast.classList.toggle("tachi-pinned");
    this.textContent = toast.classList.contains("tachi-pinned") ? "📍" : "📌";
  };

  // ── Auto-Close (3 min, unless pinned) ──
  setTimeout(() => {
    if (!toast.classList.contains("tachi-pinned")) {
      toast.remove();
    }
  }, 180000);
}

// ── Content Rendering ────────────────────────────────────────────────────────

function renderContent(text) {
  if (!text) return "";

  // Escape HTML first
  let html = text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");

  // Basic markdown-like rendering
  // Code blocks (```...```)
  html = html.replace(/```(\w*)\n([\s\S]*?)```/g, (_, lang, code) => {
    return `<pre><code class="language-${lang}">${code.trim()}</code></pre>`;
  });

  // Inline code (`...`)
  html = html.replace(/`([^`]+)`/g, "<code>$1</code>");

  // Bold (**...**)
  html = html.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");

  // Italic (*...*)
  html = html.replace(/\*([^*]+)\*/g, "<em>$1</em>");

  // Line breaks
  html = html.replace(/\n/g, "<br>");

  return html;
}
