// background.js — Tachi Chrome Extension Service Worker
//
// Manages Native Messaging connection to Tachi, context menu registration,
// and message routing between content scripts and the native host.

let port = null;
let connecting = false;
let pendingRequests = new Map(); // id -> { resolve, reject, timer }
let requestIdCounter = 0;

// ── Native Messaging Connection ──────────────────────────────────────────────

function connectTachi() {
  // Guard: prevent concurrent connection attempts (e.g., from onDisconnect
  // firing while another connectTachi() is mid-flight).
  if (connecting) return;
  connecting = true;

  // Close existing port if any
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

    // Reconnect after delay
    setTimeout(connectTachi, 2000);
  });
}

// ── Communication API ────────────────────────────────────────────────────────

function sendToTachi(action, selection, content = "") {
  return new Promise((resolve, reject) => {
    if (!port) {
      reject(new Error("Not connected to Tachi"));
      return;
    }

    const id = `tachi_${++requestIdCounter}_${Date.now()}`;
    const timer = setTimeout(() => {
      pendingRequests.delete(id);
      reject(new Error("Request timeout (30s)"));
    }, 30000);

    pendingRequests.set(id, { resolve, reject, timer });

    // Get the current active tab for thread context
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
  return false;
});

// ── Context Menus ────────────────────────────────────────────────────────────

chrome.runtime.onInstalled.addListener(() => {
  createContextMenus();
});

// Connect to Tachi native host on service worker start.
// Note: connectTachi() is NOT called inside onInstalled to avoid a race
// condition: the top-level code already calls it, and onInstalled fires
// shortly after. Calling it twice creates two Native Messaging ports in
// quick succession — the first gets disconnected, Chrome sends EOF to the
// first native host process, and it shuts down immediately.
connectTachi();

function createContextMenus() {
  // Remove old menus first to avoid duplicates on update
  chrome.contextMenus.removeAll(() => {
    const items = [
      { id: "tachi-ask",      title: "问 Tachi 🤖",         contexts: ["selection"] },
      { id: "tachi-explain",  title: "解释这个概念 📖",      contexts: ["selection"] },
      { id: "tachi-search",   title: "搜索这个 🔍",         contexts: ["selection"] },
      { id: "tachi-remember", title: "记住这个 🧠",         contexts: ["selection"] },
      { id: "tachi-recall",   title: "我读过这个吗？🔎",    contexts: ["selection"] },
    ];
    for (const item of items) {
      chrome.contextMenus.create(item);
    }
  });
}

// ── Context Menu Click Handler ───────────────────────────────────────────────

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  const actionMap = {
    "tachi-ask":      "ask_tachi",
    "tachi-explain":  "explain",
    "tachi-search":   "search",
    "tachi-remember": "remember",
    "tachi-recall":   "recall",
  };

  const action = actionMap[info.menuItemId];
  if (!action) return;

  const selection = {
    text: info.selectionText || "",
    url: tab?.url || "",
    title: tab?.title || "",
  };

  // Show loading state in content script
  if (tab?.id) {
    chrome.tabs.sendMessage(tab.id, {
      type: "show_loading",
      action,
    }).catch(() => {
      // Content script may not be loaded; ignore
    });
  }

  try {
    const result = await sendToTachi(action, selection);
    console.log("DEBUG: got result from Tachi, sending to tab and extension pages");

    // 1) Try showing result in the page as a toast (content script)
    let toastShown = false;
    if (tab?.id) {
      try {
        await chrome.tabs.sendMessage(tab.id, {
          type: "show_result",
          action,
          content: result.content,
        });
        toastShown = true;
        console.log("DEBUG: toast shown in tab", tab.id);
      } catch (e) {
        console.log("DEBUG: tabs.sendMessage failed:", e.message);
      }
    }

    // 2) Store result in storage for extension pages to pick up
    chrome.storage.local.set({
      lastResult: {
        type: "show_result",
        action,
        content: result.content,
        timestamp: Date.now(),
      }
    }).catch(() => {});

    // 3) If no toast was shown, log to console as fallback
    if (!toastShown) {
      console.log("Tachi result:", result.content);
    }
  } catch (err) {
    console.error("Tachi error:", err);

    // Show error in content script if possible
    if (tab?.id) {
      chrome.tabs.sendMessage(tab.id, {
        type: "show_error",
        content: err.message,
      }).catch(() => {});
    }
  }
});

// ── Heartbeat ────────────────────────────────────────────────────────────────

// Prevent service worker from being suspended
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
