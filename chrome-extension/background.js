// background.js — Tachi Chrome Extension Service Worker
//
// Manages Native Messaging connection to Tachi and routes requests
// from the side panel to the native host. Stores page content per tab
// for contextual follow-up conversations.

let port = null;
let connecting = false;
let pendingRequests = new Map(); // id -> { resolve, reject, timer }
let requestIdCounter = 0;

// pageContexts stores extracted page content per tab for follow-up context.
// Key: tab ID, Value: { title, url, content }
const pageContexts = new Map();

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

// ── Toolbar Icon: open side panel ───────────────────────────────────────────

chrome.action.onClicked.addListener((tab) => {
  // Open side panel for the current window
  if (tab?.windowId) {
    chrome.sidePanel.open({ windowId: tab.windowId });
  }

  // If the side panel is already open, tell it to refresh
  chrome.runtime.sendMessage({ type: "sidepanel_refresh" }).catch(() => {
    // Side panel not loaded yet — it'll auto-summarize on its own
  });
});

// ── Side Panel Message Router ────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg.type === "get_page_summary") {
    handleGetPageSummary(sender, sendResponse);
    return true;
  }

  if (msg.type === "ask_followup") {
    handleAskFollowup(msg.question, sender, sendResponse);
    return true;
  }

  if (msg.type === "connection_status") {
    sendResponse({ connected: port !== null });
    return true;
  }

  return false;
});

// ── Page Summary Handler ─────────────────────────────────────────────────────

async function handleGetPageSummary(sender, sendResponse) {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.id) {
      sendResponse({ error: "无法获取当前标签页" });
      return;
    }

    // Extract page content via content script
    let pageData;
    try {
      pageData = await chrome.tabs.sendMessage(tab.id, { type: "get_page_content" });
    } catch (e) {
      if (e.message.includes("Receiving end does not exist") ||
          e.message.includes("Could not establish connection")) {
        sendResponse({
          error: "请刷新当前页面后重试（扩展已更新，需要重新加载页面才能读取内容）",
        });
      } else {
        sendResponse({ error: `无法读取页面内容: ${e.message}` });
      }
      return;
    }

    if (!pageData || !pageData.content) {
      sendResponse({ error: "页面内容为空，可能是不支持的页面（如 chrome:// 或扩展页面）" });
      return;
    }

    // Store context for follow-up conversations
    pageContexts.set(tab.id, {
      title: pageData.title,
      url: pageData.url,
      content: pageData.content,
    });

    // Summarize via Tachi
    const prompt = buildSummarizePrompt(pageData);
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

// ── Follow-up Handler ────────────────────────────────────────────────────────

async function handleAskFollowup(question, sender, sendResponse) {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.id) {
      sendResponse({ error: "无法获取当前标签页" });
      return;
    }

    // Send the question to Tachi. The conversation context (page content
    // + previous messages) is maintained by tachi via the tab_<id> thread.
    const result = await sendToTachi("followup", question, {
      text: "",
      url: tab.url || "",
      title: tab.title || "",
    });

    sendResponse({ content: result.content });
  } catch (err) {
    console.error("Tachi followup error:", err);
    sendResponse({ error: err.message });
  }
}

// ── Prompt Builder ───────────────────────────────────────────────────────────

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

connectTachi();

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
