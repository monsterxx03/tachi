// sidepanel.js — Tachi Side Panel
//
// Chat interface for page summarization, follow-up conversation,
// and in-page translation.
//
// Communicates with background.js via chrome.runtime.sendMessage.

import { marked } from "marked";

// ── DOM Elements ─────────────────────────────────────────────────────────────

const messages = document.getElementById("messages");
const input = document.getElementById("input");
const sendBtn = document.getElementById("sendBtn");
const btnRefresh = document.getElementById("btnRefresh");
const btnTranslate = document.getElementById("btnTranslate");
const pageUrl = document.getElementById("pageUrl");
const initialLoading = document.getElementById("initialLoading");
const translateBar = document.getElementById("translateBar");
const transCount = document.getElementById("transCount");
const btnCloseTranslate = document.getElementById("btnCloseTranslate");

// ── State ────────────────────────────────────────────────────────────────────

let isWaiting = false;
let translateMode = null; // null | "original" | "bilingual" | "translated"

// ── Initialization ───────────────────────────────────────────────────────────

document.addEventListener("DOMContentLoaded", () => {
  setupListeners();
  triggerSummary();
});

function setupListeners() {
  // Send on button click
  sendBtn.addEventListener("click", handleSend);

  // Send on Enter (Shift+Enter for newline)
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  });

  // Auto-resize textarea
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 120) + "px";
  });

  // Refresh button
  btnRefresh.addEventListener("click", () => {
    clearMessages();
    triggerSummary();
  });

  // Translate button
  btnTranslate.addEventListener("click", () => {
    triggerTranslation();
  });

  // Mode toggle buttons in translate bar
  document.querySelectorAll(".mode-btn[data-mode]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const mode = btn.getAttribute("data-mode");
      setTranslateModeUI(mode);
      // Tell background to update content script
      chrome.runtime.sendMessage({
        type: "set_translate_mode",
        mode: mode,
      }).catch((err) => {
        console.error("Tachi: set translate mode error:", err);
      });
    });
  });

  // Close translation
  btnCloseTranslate.addEventListener("click", () => {
    disableTranslation();
  });

  // Listen for messages from background/content script
  chrome.runtime.onMessage.addListener((msg) => {
    switch (msg.type) {
      case "sidepanel_refresh":
        clearMessages();
        triggerSummary();
        break;
      case "translation_mode_changed":
        // Floating toggle button was clicked in content script
        if (msg.mode) {
          setTranslateModeUI(msg.mode);
        }
        break;
    }
  });
}

// ── Summary Flow ─────────────────────────────────────────────────────────────

async function triggerSummary() {
  showLoading(true);
  setInputEnabled(false);
  pageUrl.textContent = "—";

  // Reset translate bar
  hideTranslateBar();

  try {
    const response = await chrome.runtime.sendMessage({ type: "get_page_summary" });

    showLoading(false);

    if (response && response.error) {
      addMessage("error", response.error);
      setInputEnabled(true);
      return;
    }

    if (response && response.summary) {
      pageUrl.textContent = response.url || response.title || "";
      pageUrl.title = response.url || "";
      addMessage("assistant", response.summary);
      setInputEnabled(true);
    } else {
      addMessage("error", "Tachi 返回了空结果");
      setInputEnabled(true);
    }
  } catch (err) {
    showLoading(false);
    addMessage("error", `通信失败: ${err.message}`);
    setInputEnabled(true);
  }
}

// ═══════════════════════════════════════════════════════════════════════════════
// TRANSLATION FLOW
// ═══════════════════════════════════════════════════════════════════════════════

async function triggerTranslation() {
  if (isWaiting) return;
  isWaiting = true;

  addMessage("translate-info", "🌐 正在翻译页面，请稍候…");
  btnTranslate.classList.add("active");

  try {
    const response = await chrome.runtime.sendMessage({ type: "translate_page" });

    // Remove the "translating" info message
    const lastMsg = messages.lastElementChild;
    if (lastMsg && lastMsg.classList.contains("translate-info")) {
      lastMsg.remove();
    }

    if (response && response.error) {
      addMessage("error", `翻译失败: ${response.error}`);
      btnTranslate.classList.remove("active");
      return;
    }

    if (response && response.ok) {
      showTranslateBar(response.count || 0);
      setTranslateModeUI("bilingual");
      addMessage("translate-info", `✅ 已翻译 ${response.count || ""} 段`);
    } else {
      addMessage("error", `翻译返回了未知结果: ${JSON.stringify(response)}`);
      btnTranslate.classList.remove("active");
    }
  } catch (err) {
    const lastMsg = messages.lastElementChild;
    if (lastMsg && lastMsg.classList.contains("translate-info")) {
      lastMsg.remove();
    }
    addMessage("error", `翻译失败: ${err.message}`);
    btnTranslate.classList.remove("active");
  } finally {
    isWaiting = false;
    setInputEnabled(true);
  }
}

// ── Translate Mode UI ────────────────────────────────────────────────────────

function setTranslateModeUI(mode) {
  translateMode = mode;

  // Update button states
  document.querySelectorAll(".mode-btn[data-mode]").forEach((btn) => {
    const btnMode = btn.getAttribute("data-mode");
    btn.classList.toggle("active", btnMode === mode);
  });

  // Update translate button icon in header to reflect current mode
  const modeIcons = {
    original: "📖",
    bilingual: "🌐",
    translated: "🌍",
  };
  btnTranslate.textContent = modeIcons[mode] || "🌐";
  btnTranslate.classList.add("active");
}

// ── Show/Hide Translate Bar ─────────────────────────────────────────────────

function showTranslateBar(count) {
  translateBar.classList.add("visible");
  if (count) {
    transCount.textContent = `${count} 段`;
  }
}

function hideTranslateBar() {
  translateBar.classList.remove("visible");
  transCount.textContent = "";
  translateMode = null;
  btnTranslate.classList.remove("active");
  btnTranslate.textContent = "🌐";
}

// ── Disable Translation ──────────────────────────────────────────────────────

async function disableTranslation() {
  hideTranslateBar();

  try {
    await chrome.runtime.sendMessage({ type: "disable_translation" });
  } catch (err) {
    console.error("Tachi: disable translation error:", err);
  }

  addMessage("translate-info", "已关闭页面翻译");
}

// ── Send Flow ────────────────────────────────────────────────────────────────

async function handleSend() {
  const text = input.value.trim();
  if (!text || isWaiting) return;

  // Add user message
  addMessage("user", text);
  input.value = "";
  input.style.height = "auto";

  // Show typing indicator
  const typing = showTyping();
  setInputEnabled(false);

  try {
    const response = await chrome.runtime.sendMessage({
      type: "ask_followup",
      question: text,
    });

    removeTyping(typing);
    setInputEnabled(true);

    if (response && response.error) {
      addMessage("error", response.error);
    } else if (response && response.content) {
      addMessage("assistant", response.content);
    } else {
      addMessage("error", "Tachi 返回了空结果");
    }
  } catch (err) {
    removeTyping(typing);
    setInputEnabled(true);
    addMessage("error", `通信失败: ${err.message}`);
  }
}

// ── Message Rendering ────────────────────────────────────────────────────────

function addMessage(role, content) {
  const div = document.createElement("div");
  div.className = `msg ${role}`;

  if (role === "assistant") {
    div.innerHTML = marked.parse(content);
  } else {
    div.textContent = content;
  }

  messages.appendChild(div);
  div.scrollIntoView({ behavior: "smooth" });
}

function clearMessages() {
  const children = messages.querySelectorAll(".msg, .typing-indicator");
  children.forEach((c) => c.remove());
}

// ── UI Helpers ───────────────────────────────────────────────────────────────

function showLoading(show) {
  initialLoading.style.display = show ? "block" : "none";
}

function showTyping() {
  const div = document.createElement("div");
  div.className = "typing-indicator";
  div.innerHTML = "<span></span><span></span><span></span>";
  messages.appendChild(div);
  div.scrollIntoView({ behavior: "smooth" });
  return div;
}

function removeTyping(el) {
  if (el && el.parentNode) el.remove();
}

function setInputEnabled(enabled) {
  isWaiting = !enabled;
  input.disabled = !enabled;
  sendBtn.disabled = !enabled;
  if (enabled) input.focus();
}
