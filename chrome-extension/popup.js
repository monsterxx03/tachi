// popup.js — Tachi Popup Panel Logic

let port = null;

// ── Initialization ──

document.addEventListener("DOMContentLoaded", () => {
  updateStatus("connecting", "连接中…");

  // Try connecting to native host
  connectTachi();

  // Button handlers
  document.querySelectorAll("[data-action]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const action = btn.dataset.action;
      handleAction(action);
    });
  });

  // Send button
  document.getElementById("sendBtn").addEventListener("click", () => {
    const text = document.getElementById("queryInput").value.trim();
    if (text) {
      handleAction("ask_tachi", text);
      document.getElementById("queryInput").value = "";
    }
  });

  // Enter key in textarea
  document.getElementById("queryInput").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      document.getElementById("sendBtn").click();
    }
  });

  // Open side panel
  document.getElementById("openPanel").addEventListener("click", (e) => {
    e.preventDefault();
    // Try opening side panel directly from popup context.
    chrome.windows.getCurrent((win) => {
      if (win?.id) {
        chrome.sidePanel.open({ windowId: win.id }).catch(() => {
          // Fallback: send message to background
          chrome.runtime.sendMessage({ type: "open_panel" });
        });
      }
    });
    window.close();
  });
});

// ── Native Messaging ──

function connectTachi() {
  try {
    port = chrome.runtime.connectNative("com.tachi.chrome");

    port.onMessage.addListener((msg) => {
      // Popup handles responses directly
      if (msg.type === "result") {
        updateStatus("connected", "已连接");
        showResult(msg.content);
      } else if (msg.type === "error") {
        updateStatus("connected", "已连接");
        showError(msg.content);
      }
    });

    port.onDisconnect.addListener(() => {
      port = null;
      updateStatus("connecting", "连接断开，重试中…");
      setTimeout(connectTachi, 2000);
    });

    updateStatus("connected", "已连接");
  } catch (e) {
    updateStatus("error", `连接失败: ${e.message}`);
  }
}

// ── Action Handling ──

function handleAction(action, extraContent = "") {
  if (!port) {
    showError("未连接到 Tachi。请确保 Tachi 正在运行。");
    return;
  }

  const id = `popup_${Date.now()}`;
  port.postMessage({
    id,
    action,
    threadID: `popup_${Date.now()}`,
    selection: {
      text: extraContent || getSelectionText(),
      url: "",
      title: document.title,
    },
    content: extraContent || "",
  });
}

function getSelectionText() {
  // Try to get text from the active tab
  return "（来自弹窗）";
}

// ── UI Updates ──

function updateStatus(state, text) {
  const dot = document.getElementById("statusDot");
  const textEl = document.getElementById("statusText");
  const infoEl = document.getElementById("connectionInfo");

  dot.className = "status-dot";
  if (state === "connected") dot.classList.add("connected");
  if (state === "connecting") dot.classList.add("connecting");

  textEl.textContent = text;
  infoEl.textContent = text;
}

function showResult(content) {
  const area = document.getElementById("resultArea");
  const contentEl = document.getElementById("resultContent");
  if (area && contentEl) {
    contentEl.textContent = content;
    area.classList.add("visible");
    // Auto-scroll to show result
    area.scrollTop = area.scrollHeight;
  }
}

function showError(message) {
  const area = document.getElementById("resultArea");
  const contentEl = document.getElementById("resultContent");
  if (area && contentEl) {
    contentEl.textContent = `❌ ${message}`;
    area.classList.add("visible");
  }
}
