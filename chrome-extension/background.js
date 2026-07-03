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

// translateRequest tracks selection translation results by ID for popup delivery.
// Using a Map instead of a single `latestTranslation` avoids race conditions
// when the user triggers multiple translations in quick succession.
let translateReqId = 0;
const translationResults = new Map(); // reqId -> { original, translation, error }

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
    // Proactive messages (no matching pending request) are silently ignored.
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

// ── Context Menus ────────────────────────────────────────────────────────────

function createContextMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "translate_page",
      title: "🌐 翻译当前页面",
      contexts: ["page"],
    });

    chrome.contextMenus.create({
      id: "translate_selection",
      title: "🌐 翻译选中文字",
      contexts: ["selection"],
    });
  });
}

// ── Context Menu Click Handler ───────────────────────────────────────────────

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === "translate_page") {
    // Trigger page translation (same as side panel 🌐 button)
    if (!tab || !tab.id) return;

    try {
      chrome.tabs.sendMessage(tab.id, { type: "show_trans_progress" }).catch(() => {});

      const paraData = await chrome.tabs.sendMessage(tab.id, { type: "extract_paragraphs" });

      if (!paraData || !paraData.paragraphs || paraData.paragraphs.length === 0) {
        chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});
        return;
      }

      const prompt = buildTranslationPrompt(paraData.paragraphs, tab);
      const result = await sendToTachi("translate", prompt, {
        text: "", url: tab.url || "", title: tab.title || "",
      });

      const translations = parseTranslationResult(result.content, paraData.paragraphs.length);
      if (translations && translations.length > 0) {
        await chrome.tabs.sendMessage(tab.id, {
          type: "inject_translations",
          translations: translations,
        });
      }

      chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});
    } catch (err) {
      console.error("Tachi context menu translate page error:", err);
      chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});
    }
  }

  if (info.menuItemId === "translate_selection") {
    // Translate selected text and show in popup
    const selectedText = info.selectionText;
    if (!selectedText || !selectedText.trim()) return;

    const reqId = ++translateReqId;

    try {
      const lang = detectPageLanguage([selectedText]);
      const instruction = (lang === "zh" || lang === "ja" || lang === "ko")
        ? "Translate the following text to English.\nReturn ONLY the translation, no explanation."
        : "请将以下文字翻译成中文（简体）。\n只返回翻译结果，不要任何解释。";

      const prompt = `${instruction}\n\n${selectedText}`;
      const result = await sendToTachi("translate", prompt, {
        text: selectedText,
        url: tab?.url || "",
        title: tab?.title || "",
      });

      translationResults.set(reqId, {
        original: selectedText,
        translation: result.content,
      });

      // Auto-cleanup after 60s
      setTimeout(() => translationResults.delete(reqId), 60000);

      chrome.windows.create({
        url: chrome.runtime.getURL(`translate-popup.html?id=${reqId}`),
        type: "popup",
        width: 480,
        height: 350,
        focused: true,
      });
    } catch (err) {
      translationResults.set(reqId, {
        original: selectedText,
        translation: null,
        error: err.message,
      });

      setTimeout(() => translationResults.delete(reqId), 60000);

      chrome.windows.create({
        url: chrome.runtime.getURL(`translate-popup.html?id=${reqId}`),
        type: "popup",
        width: 480,
        height: 200,
        focused: true,
      });
    }
  }
});

// ── Side Panel Message Router ────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  switch (msg.type) {
    case "get_page_summary":
      handleGetPageSummary(sender, sendResponse);
      return true;

    case "ask_followup":
      handleAskFollowup(msg.question, sender, sendResponse);
      return true;

    case "translate_page":
      handleTranslatePage(sender, sendResponse);
      return true;

    case "set_translate_mode":
      handleSetTranslateMode(msg.mode, sender, sendResponse);
      return true;

    case "disable_translation":
      handleDisableTranslation(sender, sendResponse);
      return true;

    case "connection_status":
      sendResponse({ connected: ws !== null && ws.readyState === WebSocket.OPEN });
      return true;

    case "get_latest_translation":
      {
        const reqId = msg.requestId;
        const result = reqId ? translationResults.get(reqId) : null;
        if (result) {
          sendResponse({
            original: result.original,
            translation: result.translation,
            error: result.error || null,
          });
          // Cleanup after delivery
          translationResults.delete(reqId);
        } else {
          sendResponse({ error: "没有找到翻译结果" });
        }
        return true;
      }
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

// ═══════════════════════════════════════════════════════════════════════════════
// TRANSLATION HANDLER
// ═══════════════════════════════════════════════════════════════════════════════

async function handleTranslatePage(sender, sendResponse) {
  let responded = false;
  const respond = (data) => { if (!responded) { responded = true; sendResponse(data); } };

  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.id) {
      respond({ error: "无法获取当前标签页" });
      return;
    }

    // Step 1: Tell content script to show progress
    chrome.tabs.sendMessage(tab.id, { type: "show_trans_progress" }).catch(() => {});

    // Step 2: Extract paragraphs from the page
    let paraData;
    try {
      paraData = await chrome.tabs.sendMessage(tab.id, { type: "extract_paragraphs" });
    } catch (e) {
      chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});
      if (e.message.includes("Receiving end does not exist")) {
        respond({ error: "请刷新当前页面后重试（扩展已更新，需要重新加载页面才能读取内容）" });
      } else {
        respond({ error: `无法读取页面段落: ${e.message}` });
      }
      return;
    }

    if (!paraData || !paraData.paragraphs || paraData.paragraphs.length === 0) {
      chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});
      respond({ error: "页面中没有找到可翻译的文本段落" });
      return;
    }

    // Step 3: Send paragraphs to Tachi for translation
    const prompt = buildTranslationPrompt(paraData.paragraphs, tab);
    const result = await sendToTachi("translate", prompt, {
      text: "",
      url: tab.url || "",
      title: tab.title || "",
    });

    // Step 4: Parse the translation result (expecting a JSON array)
    const translations = parseTranslationResult(result.content, paraData.paragraphs.length);

    if (!translations || translations.length === 0) {
      chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});
      respond({ error: "翻译失败：无法解析翻译结果" });
      return;
    }

    // Step 5: Send translations back to content script for injection
    await chrome.tabs.sendMessage(tab.id, {
      type: "inject_translations",
      translations: translations,
    });

    // Hide progress
    chrome.tabs.sendMessage(tab.id, { type: "hide_trans_progress" }).catch(() => {});

    respond({
      ok: true,
      count: translations.length,
      mode: "bilingual",
    });
  } catch (err) {
    // Hide progress on error
    chrome.tabs
      .query({ active: true, currentWindow: true })
      .then((tabs) => {
        if (tabs[0]?.id) {
          chrome.tabs.sendMessage(tabs[0].id, { type: "hide_trans_progress" }).catch(() => {});
        }
      })
      .catch(() => {});

    console.error("Tachi translate error:", err);
    respond({ error: err.message });
  }
}

// ── Translation Prompt Builder ───────────────────────────────────────────────

function buildTranslationPrompt(paragraphs, tab) {
  const totalLen = paragraphs.reduce((sum, p) => sum + p.length, 0);
  const lang = detectPageLanguage(paragraphs);

  let instruction = "";
  if (lang === "zh" || lang === "ja" || lang === "ko") {
    instruction = "Translate the following paragraphs to English.";
    instruction += "\nReturn ONLY a valid JSON array of strings where each element is the translation of the corresponding paragraph.\nNo explanation, no markdown formatting, no code block fences. Just pure JSON array.";
  } else {
    instruction = "请将以下段落逐段翻译成中文（简体）。";
    instruction += "\n只返回一个 JSON 数组，每个元素是对应段落的翻译。不要任何解释、Markdown 格式或代码块标记，只返回纯 JSON 数组。";
  }

  // For very large pages, split context: tell the LLM the page title/URL for context
  let contextInfo = "";
  if (tab?.title) {
    contextInfo = `\n\nPage title: ${tab.title}`;
  }
  if (tab?.url) {
    contextInfo += `\nPage URL: ${tab.url}`;
  }

  return `${instruction}${contextInfo}

Paragraphs to translate:
${JSON.stringify(paragraphs)}

Output (pure JSON array):`;
}

// ── Detect Page Language ─────────────────────────────────────────────────────

function detectPageLanguage(paragraphs) {
  // Sample more text for reliable detection (first 20 paragraphs or 2000 chars)
  const sample = paragraphs.slice(0, 20).join(" ").slice(0, 2000);
  const cjkCount = (sample.match(/[\u4e00-\u9fff\u3400-\u4dbf\uf900-\ufaff]/g) || []).length;
  const totalChars = sample.replace(/\s/g, "").length;
  if (totalChars === 0) return "en";
  // Higher threshold (0.15) to reduce false positives from mixed-language content
  return cjkCount / totalChars > 0.15 ? "zh" : "en";
}

// ── Parse Translation Result ─────────────────────────────────────────────────

function parseTranslationResult(content, expectedCount) {
  if (!content) return null;

  let trimmed = content.trim();

  // Remove markdown code block fences if present
  trimmed = trimmed.replace(/^```(?:json)?\s*\n?/i, "").replace(/\n?```\s*$/i, "").trim();

  // Try to find a JSON array in the response
  const arrayMatch = trimmed.match(/\[[\s\S]*\]/);
  if (arrayMatch) {
    try {
      const parsed = JSON.parse(arrayMatch[0]);
      if (Array.isArray(parsed) && parsed.length > 0) {
        if (parsed.every((item) => typeof item === "string")) {
          return parsed;
        }
        return parsed.map((item) => String(item));
      }
    } catch (e) {
      // JSON parse failed
    }
  }

  // Fallback: try line-by-line parsing (numbered list)
  // Only use this if we have a reasonable number of lines and they look like list items
  const lines = trimmed.split("\n").filter((l) => l.trim());
  if (lines.length >= 2 && lines.length <= expectedCount * 1.5) {
    const translations = [];
    for (const line of lines) {
      const cleaned = line
        .replace(/^\[\d+\]\s*/, "")
        .replace(/^\d+[\.\)]\s*/, "")
        .replace(/^["']|["']$/g, "")
        .trim();
      if (cleaned && cleaned.length > 2) {
        translations.push(cleaned);
      }
    }
    if (translations.length > 0 && translations.length >= expectedCount * 0.5) {
      return translations;
    }
  }

  // If expectedCount is 1, treat the whole response as one translation
  if (expectedCount === 1 && trimmed.length > 0) {
    return [trimmed];
  }

  return null;
}

// ── Set Translate Mode Handler ───────────────────────────────────────────────

async function handleSetTranslateMode(mode, sender, sendResponse) {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.id) {
      sendResponse({ error: "无法获取当前标签页" });
      return;
    }

    const result = await chrome.tabs.sendMessage(tab.id, {
      type: "set_translate_mode",
      mode: mode,
    });

    sendResponse({ ok: true, mode: result?.mode || mode });
  } catch (err) {
    console.error("Tachi set translate mode error:", err);
    sendResponse({ error: err.message });
  }
}

// ── Disable Translation Handler ──────────────────────────────────────────────

async function handleDisableTranslation(sender, sendResponse) {
  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.id) {
      sendResponse({ error: "无法获取当前标签页" });
      return;
    }

    await chrome.tabs.sendMessage(tab.id, { type: "disable_translation" });
    sendResponse({ ok: true });
  } catch (err) {
    console.error("Tachi disable translation error:", err);
    sendResponse({ error: err.message });
  }
}

// ── Prompt Builder (Summary) ─────────────────────────────────────────────────

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
createContextMenus();

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
