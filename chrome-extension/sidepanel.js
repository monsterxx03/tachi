// sidepanel.js — Tachi Side Panel
//
// Chat interface for page summarization and follow-up conversation.
// On load, auto-triggers page summarization. User can ask follow-up
// questions with full page context maintained by tachi.
//
// Communicates with background.js via chrome.runtime.sendMessage.

import { marked } from "marked";

// ── DOM Elements ─────────────────────────────────────────────────────────────

const messages = document.getElementById("messages");
const input = document.getElementById("input");
const sendBtn = document.getElementById("sendBtn");
const btnRefresh = document.getElementById("btnRefresh");
const pageUrl = document.getElementById("pageUrl");
const initialLoading = document.getElementById("initialLoading");

// ── State ────────────────────────────────────────────────────────────────────

let isWaiting = false; // true while waiting for a response

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

  // Listen for refresh signal from background (when icon is clicked
  // while the side panel is already open)
  chrome.runtime.onMessage.addListener((msg) => {
    if (msg.type === "sidepanel_refresh") {
      clearMessages();
      triggerSummary();
    }
  });
}

// ── Summary Flow ─────────────────────────────────────────────────────────────

async function triggerSummary() {
  showLoading(true);
  setInputEnabled(false);
  pageUrl.textContent = "—";

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
  // Remove all message elements, keeping the loading placeholder
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
