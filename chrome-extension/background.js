// background.js — Tachi Chrome Extension Service Worker
//
// Manages Native Messaging connection to Tachi and routes requests
// from the popup (and potentially other extension pages) to the native host.

let port = null;
let connecting = false;
let pendingRequests = new Map(); // id -> { resolve, reject, timer }
let requestIdCounter = 0;

// ── Native Messaging Connection ──────────────────────────────────────────────

function connectTachi() {
  if (connecting) return;
  connecting = true;

  if (port) {
    try { port.disconnect(); } catch (e) { /* ignore */ }
  }

  try {
    port = chrome.runtime.connectNative("com.tachi.chrome");
  } catch (e) {
    console.error("Tachi connectNative failed:", e.message);
    connecting = false;
    setTimeout(connectTachi, 5000);
    return;
  }

  if (!port) {
    console.error("Tachi connectNative returned null");
    connecting = false;
    setTimeout(connectTachi, 5000);
    return;
  }

  connecting = false;

  port.onMessage.addListener((msg) => {
    const pending = pendingRequests.get(msg.id);
    if (pending) {
      clearTimeout(pending.timer);
      pendingRequests.delete(msg.id);
      if (msg.type === "error") {
        pending.reject(new Error(msg.content));
      } else {
        pending.resolve(msg);
      }
    }
  });

  port.onDisconnect.addListener(() => {
    const error = chrome.runtime.lastError;
    console.log("Tachi native host disconnected:", error?.message || "unknown");
    port = null;

    // Reject all pending requests
    for (const [id, pending] of pendingRequests) {
      clearTimeout(pending.timer);
      pending.reject(new Error("Native host disconnected"));
    }
    pendingRequests.clear();

    setTimeout(connectTachi, 2000);
  });
}

// ── Communication API ────────────────────────────────────────────────────────

function sendToTachi(action, content = "", selection = {}) {
  return new Promise((resolve, reject) => {
    if (!port) {
      reject(new Error("Not connected to Tachi. Make sure Tachi is running with: tachi channel"));
      return;
    }

    const id = `tachi_${++requestIdCounter}_${Date.now()}`;
    const timer = setTimeout(() => {
      pendingRequests.delete(id);
      reject(new Error("Request timeout (30s)"));
    }, 30000);

    pendingRequests.set(id, { resolve, reject, timer });

    chrome.tabs.query({ active: true, currentWindow: true }, (tabs) => {
      const tab = tabs[0];
      port.postMessage({
        id,
        action,
        threadID: tab ? `tab_${tab.id}` : "global",
        selection: {
          text: selection.text || "",
          url: selection.url || (tab ? tab.url : ""),
          title: selection.title || (tab ? tab.title : ""),
        },
        content: content || "",
      });
    });
  });
}

// ── Runtime Message Handler ──────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === "summarize_page") {
    handleSummarizePage(sender, sendResponse);
    return true; // keep channel open for async response
  }

  if (msg.type === "connection_status") {
    sendResponse({ connected: port !== null });
    return true;
  }

  return false;
});

async function handleSummarizePage(sender, sendResponse) {
  try {
    // Get the active tab
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.id) {
      sendResponse({ error: "无法获取当前标签页" });
      return;
    }

    // Request page content from the content script
    let pageData;
    try {
      pageData = await chrome.tabs.sendMessage(tab.id, { type: "get_page_content" });
    } catch (e) {
      sendResponse({ error: `无法读取页面内容: ${e.message}` });
      return;
    }

    if (!pageData || !pageData.content) {
      sendResponse({ error: "页面内容为空，可能是不支持的页面（如 chrome:// 或扩展页面）" });
      return;
    }

    // Build a summarization prompt with the page content
    const prompt = buildSummarizePrompt(pageData);

    // Send to Tachi
    const result = await sendToTachi("summarize", prompt, {
      text: "",
      url: pageData.url,
      title: pageData.title,
    });

    sendResponse({
      title: pageData.title,
      url: pageData.url,
      summary: result.content,
    });
  } catch (err) {
    console.error("Tachi summarize error:", err);
    sendResponse({ error: err.message });
  }
}

function buildSummarizePrompt(pageData) {
  const contentLength = pageData.content.length;
  const sourceInfo = [];
  if (pageData.byline) sourceInfo.push(`作者: ${pageData.byline}`);
  sourceInfo.push(`字符数: ${contentLength}`);

  return `请用中文总结以下网页的核心内容。结构要求：
1. 先给一个一句话概括（不超过50字）
2. 然后列出 3-5 个要点（每个要点一行，子弹列表格式）
3. 最后如果有重要的数据/数字/日期，单独列出

---
网页标题: ${pageData.title}
网页地址: ${pageData.url}
${sourceInfo.join(" | ")}
---

${pageData.content}`;
}

// ── Heartbeat ────────────────────────────────────────────────────────────────

// Connect on startup
connectTachi();

// Keep service worker alive and connection healthy
setInterval(() => {
  if (port) {
    try {
      port.postMessage({ action: "ping", id: "ping", threadID: "global" });
    } catch (e) {
      console.log("Ping failed, reconnecting...");
      connectTachi();
    }
  } else {
    connectTachi();
  }
}, 25000);
