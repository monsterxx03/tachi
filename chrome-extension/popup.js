// popup.js — Tachi Popup Panel Logic
//
// Single-action popup: "总结当前页面" sends a request to the background
// service worker, which extracts page content and forwards to Tachi for
// LLM summarization. Result is rendered as Markdown via marked.

import { marked } from "marked";

// ── Initialization ───────────────────────────────────────────────────────────

document.addEventListener("DOMContentLoaded", () => {
  checkConnection();

  document.getElementById("summarizeBtn").addEventListener("click", onSummarize);
});

async function checkConnection() {
  try {
    const resp = await chrome.runtime.sendMessage({ type: "connection_status" });
    if (resp && resp.connected) {
      updateStatus("connected", "已连接");
    } else {
      updateStatus("connecting", "等待连接…");
    }
  } catch (e) {
    updateStatus("connecting", "等待连接…");
  }
}

// ── Summarize Flow ───────────────────────────────────────────────────────────

async function onSummarize() {
  const btn = document.getElementById("summarizeBtn");
  const resultArea = document.getElementById("resultArea");
  const loadingArea = document.getElementById("loadingArea");

  // Reset UI
  resultArea.classList.remove("visible");
  resultArea.innerHTML = `<div class="result-title" id="resultTitle"></div>
    <div class="result-meta" id="resultMeta"></div>
    <div id="resultContent"></div>`;

  // Show loading
  btn.disabled = true;
  btn.textContent = "⏳ 分析中…";
  loadingArea.style.display = "block";

  try {
    const response = await chrome.runtime.sendMessage({ type: "summarize_page" });

    // Hide loading
    loadingArea.style.display = "none";
    btn.disabled = false;
    btn.textContent = "📄 总结当前页面";

    if (response && response.error) {
      showError(response.error);
      return;
    }

    if (response && response.summary) {
      showResult(response);
    } else {
      showError("Tachi 返回了空结果");
    }
  } catch (err) {
    loadingArea.style.display = "none";
    btn.disabled = false;
    btn.textContent = "📄 总结当前页面";
    showError(err.message || "与 Tachi 通信失败");
  }
}

// ── UI Updates ───────────────────────────────────────────────────────────────

function updateStatus(state, text) {
  const dot = document.getElementById("statusDot");
  const textEl = document.getElementById("statusText");

  dot.className = "status-dot";
  if (state === "connected") dot.classList.add("connected");
  if (state === "connecting") dot.classList.add("connecting");

  textEl.textContent = text;
}

function showResult(data) {
  const area = document.getElementById("resultArea");
  const titleEl = document.getElementById("resultTitle");
  const metaEl = document.getElementById("resultMeta");
  const contentEl = document.getElementById("resultContent");

  titleEl.textContent = data.title || "页面总结";
  metaEl.innerHTML = `<a href="${escapeHtml(data.url || "")}" target="_blank">${escapeHtml(data.url || "")}</a>`;
  contentEl.innerHTML = marked.parse(data.summary || "");

  area.classList.add("visible");
  area.scrollTop = 0;
}

function showError(message) {
  const area = document.getElementById("resultArea");
  area.innerHTML = `<div style="color:var(--error)">❌ ${escapeHtml(message)}</div>
    <div style="margin-top:8px;font-size:12px;color:var(--text-secondary)">
      请确保 Tachi 正在运行：<code style="background:var(--border);padding:1px 4px;border-radius:3px;">tachi channel</code>
    </div>`;
  area.classList.add("visible");
}

// ── Utilities ─────────────────────────────────────────────────────────────────

function escapeHtml(text) {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}
