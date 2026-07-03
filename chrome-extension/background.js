// background.js — Tachi Chrome Extension Service Worker
//
// Manages a WebSocket connection to the local Tachi HTTP server and routes
// requests from the side panel. Stores page content per tab for contextual
// follow-up conversations.
//
// Tachi must be running in channel mode:
//   tachi channel

const TACHI_WS_URL = "ws://127.0.0.1:18520/ws";

let ws = null;
let reconnectTimer = null;
let pendingRequests = new Map(); // id -> { resolve, reject }
let requestIdCounter = 0;

// pageContexts stores extracted page content per tab for follow-up context.
// Key: tab ID, Value: { title, url, content }
const pageContexts = new Map();

// ── WebSocket Connection ─────────────────────────────────────────────────────

function connect() {
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
    return;
  }

  clearTimeout(reconnectTimer);

  try {
    ws = new WebSocket(TACHI_WS_URL);
  } catch (e) {
    console.error("Tachi: WebSocket creation failed:", e.message);
    scheduleReconnect();
    return;
  }

  ws.onopen = () => {
    console.log("Tachi: WebSocket connected to", TACHI_WS_URL);
  };

  ws.onmessage = (event) => {
    let msg;
    try {
      msg = JSON.parse(event.data);
    } catch (e) {
      console.error("Tachi: invalid JSON from server:", e.message);
      return;
    }

    const pending = pendingRequests.get(msg.id);
    if (pending) {
      pendingRequests.delete(msg.id);
      if (msg.type === "error") {
        pending.reject(new Error(msg.content));
      } else {
        pending.resolve(msg);
      }
    }
    // Proactive messages (no matching pending request) are silently ignored
    // in the background worker. The side panel doesn't need them.
  };

  ws.onclose = (event) => {
    console.log("Tachi: WebSocket disconnected (code=" + event.code + ")", event.reason);
    ws = null;

    // Reject all pending requests.
    for (const [id, pending] of pendingRequests) {
      pending.reject(new Error("WebSocket disconnected"));
    }
    pendingRequests.clear();

    scheduleReconnect();
  };

  ws.onerror = (event) => {
    console.error("Tachi: WebSocket error");
    // onclose will fire after onerror, triggering reconnect.
  };
}

function scheduleReconnect() {
  clearTimeout(reconnectTimer);
  reconnectTimer = setTimeout(connect, 3000);
}

// ── Communication API ────────────────────────────────────────────────────────

function sendToTachi(action, content = "", selection = {}) {
  return new Promise((resolve, reject) => {
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      reject(new Error("Not connected to Tachi. Make sure Tachi is running with: tachi channel"));
      return;
    }

    const id = `tachi_${++requestIdCounter}_${Date.now()}`;

    pendingRequests.set(id, { resolve, reject });

    // Set a timeout.
    setTimeout(() => {
      if (pendingRequests.has(id)) {
        pendingRequests.delete(id);
        reject(new Error("Request timeout (30s)"));
      }
    }, 30000);

    chrome.tabs.query({ active: true, currentWindow: true }, (tabs) => {
      const tab = tabs[0];
      const msg = JSON.stringify({
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
      ws.send(msg);
    });
  });
}

// ── Toolbar Icon: open side panel ───────────────────────────────────────────

chrome.action.onClicked.addListener(async (tab) => {
  console.log("Tachi: action.onClicked fired, tab:", tab?.id, "window:", tab?.windowId);

  let windowId = tab?.windowId;
  if (!windowId) {
    try {
      const win = await chrome.windows.getCurrent();
      windowId = win.id;
    } catch (e) {
      console.error("Tachi: failed to get window ID:", e.message);
    }
  }

  if (windowId) {
    try {
      await chrome.sidePanel.open({ windowId });
      console.log("Tachi: sidePanel.open succeeded for window", windowId);
    } catch (e) {
      console.error("Tachi: sidePanel.open failed:", e.message);
    }
  } else {
    console.error("Tachi: no windowId available, cannot open side panel");
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
    sendResponse({ connected: ws !== null && ws.readyState === WebSocket.OPEN });
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

// ── Startup ──────────────────────────────────────────────────────────────────

connect();

// Ensure the side panel is globally enabled
chrome.sidePanel.setOptions({ enabled: true }).catch((e) =>
  console.error("Tachi: sidePanel.setOptions failed:", e.message)
);

// Heartbeat — keep connection alive and detect stale connections
setInterval(() => {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ action: "ping", id: "ping", threadID: "global" }));
  } else {
    connect();
  }
}, 25000);
