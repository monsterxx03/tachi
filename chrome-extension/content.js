// content.js — Tachi Chrome Extension Content Script
//
// Injected into every page. Provides:
// 1. Page content extraction using Mozilla's Readability
// 2. In-page paragraph translation (immersive-style)

import { Readability } from "@mozilla/readability";

// ── Readability: Page Content Extraction ─────────────────────────────────────

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  switch (msg.type) {
    case "get_page_content":
      sendResponse(extractPageContent());
      return true;

    case "extract_paragraphs":
      sendResponse(extractParagraphs());
      return true;

    case "inject_translations":
      injectTranslations(msg.translations);
      sendResponse({ ok: true });
      return true;

    case "set_translate_mode":
      setTranslateMode(msg.mode);
      sendResponse({ ok: true, mode: tachiTranslateMode });
      return true;

    case "disable_translation":
      cleanupTranslation();
      sendResponse({ ok: true });
      return true;

    case "show_trans_progress":
      showTranslationProgress();
      return false;

    case "hide_trans_progress":
      hideTranslationProgress();
      return false;
  }
  return false;
});

// ── Content Extraction (Readability) ─────────────────────────────────────────

const MAX_CONTENT_LENGTH = 100_000; // ~25K tokens

function extractPageContent() {
  try {
    const documentClone = document.cloneNode(true);
    const reader = new Readability(documentClone);
    const article = reader.parse();

    if (article && article.textContent && article.textContent.trim().length > 0) {
      return {
        title: article.title || document.title,
        content: truncate(article.textContent.trim(), MAX_CONTENT_LENGTH),
        url: window.location.href,
        excerpt: article.excerpt || "",
        byline: article.byline || "",
        length: article.length,
      };
    }
  } catch (e) {
    console.error("Tachi: Readability failed, falling back:", e.message);
  }

  const bodyText = (document.body?.innerText || "").replace(/\n{3,}/g, "\n\n").trim();
  return {
    title: document.title,
    content: truncate(bodyText, MAX_CONTENT_LENGTH),
    url: window.location.href,
    excerpt: "",
    byline: "",
    length: bodyText.length,
  };
}

function truncate(text, maxLength) {
  if (text.length <= maxLength) return text;
  const truncated = text.substring(0, maxLength);
  const lastPeriod = truncated.lastIndexOf("。");
  const lastNewline = truncated.lastIndexOf("\n\n");
  const cutPoint = Math.max(lastPeriod, lastNewline);
  if (cutPoint > maxLength * 0.7) {
    return truncated.substring(0, cutPoint + 1) + "\n\n[内容已截断…]";
  }
  return truncated + "\n\n[内容已截断…]";
}

// ═══════════════════════════════════════════════════════════════════════════════
// TRANSLATION FEATURE — Immersive-style in-page paragraph translation
// ═══════════════════════════════════════════════════════════════════════════════

// ── State ────────────────────────────────────────────────────────────────────

let tachiTranslationActive = false;
let tachiTranslateMode = "bilingual"; // "original" | "bilingual" | "translated"
let tachiFloatingBtn = null;
let tachiStyleEl = null;
let tachiParaElements = []; // Array of { element: HTMLElement, text: string, index: number }

// ── Paragraph Extraction ─────────────────────────────────────────────────────

function extractParagraphs() {
  // Find candidate text elements on the page.
  const candidates = document.querySelectorAll(
    "p, h1, h2, h3, h4, h5, h6, li, td, th, blockquote, figcaption, dt, dd, .content p, article p, main p"
  );

  const results = [];

  for (const el of candidates) {
    // Skip elements inside uninteresting containers
    if (["SCRIPT", "STYLE", "NOSCRIPT", "IFRAME", "SVG"].includes(el.parentElement?.tagName || "")) {
      continue;
    }

    // Skip already-processed elements (inside existing translation blocks)
    if (el.closest(".tachi-trans-block")) continue;

    // Skip hidden elements
    const style = window.getComputedStyle(el);
    if (style.display === "none" || style.visibility === "hidden") continue;

    const text = el.textContent.trim();
    if (text.length < 10) continue;
    if (/^[\d\s\W]+$/.test(text)) continue;

    // Skip child elements whose text matches a parent candidate's text exactly.
    // This handles cases like <p><strong>text</strong></p> where both would
    // match the selector but only the outermost should be translated.
    let parent = el.parentElement;
    let skip = false;
    while (parent && parent !== document.body) {
      const pTag = parent.tagName;
      if (pTag === "A" && parent.textContent.trim() === text) {
        skip = true;
        break;
      }
      if (["P", "LI", "TD", "TH", "BLOCKQUOTE"].includes(pTag) && parent.textContent.trim() === text) {
        skip = true;
        break;
      }
      parent = parent.parentElement;
    }
    if (skip) continue;

    results.push({ element: el, text, index: results.length });
  }

  // Deduplicate by exact text match to avoid double-processing identical phrases.
  // This naturally handles the common case of duplicate elements with identical text.
  // NOTE: Sibling list items with identical text (e.g., two <li> with "loading…")
  // are correctly preserved since their parent's textContent differs from theirs.
  const seenTexts = new Set();
  const finalResults = [];
  for (const item of results) {
    if (!seenTexts.has(item.text)) {
      seenTexts.add(item.text);
      finalResults.push(item);
    }
  }

  tachiParaElements = finalResults;

  return {
    paragraphs: finalResults.map((p) => p.text),
    count: finalResults.length,
  };
}

// ── Inject Translations ──────────────────────────────────────────────────────

function injectTranslations(translations) {
  if (!translations || translations.length === 0) return;

  // First cleanup any existing translation UI
  cleanupTranslationUI();

  tachiTranslationActive = true;
  injectStyles();

  // Inject translation blocks for each paragraph
  const minLen = Math.min(translations.length, tachiParaElements.length);

  for (let i = 0; i < minLen; i++) {
    const para = tachiParaElements[i];
    const transText = translations[i];
    if (!transText || !transText.trim()) continue;

    // Create translation element
    const transBlock = document.createElement("div");
    transBlock.className = "tachi-trans-block";
    transBlock.setAttribute("data-tachi-para-idx", String(i));
    transBlock.textContent = transText;

    // Insert after the original element
    para.element.parentNode?.insertBefore(transBlock, para.element.nextSibling);
  }

  // Apply current mode
  applyTranslateMode(tachiTranslateMode);

  // Add floating toggle button
  addFloatingToggle();
}

// ── Apply Translate Mode ─────────────────────────────────────────────────────

function applyTranslateMode(mode) {
  const blocks = document.querySelectorAll(".tachi-trans-block");

  switch (mode) {
    case "original":
      // Hide translations, show originals
      blocks.forEach((b) => {
        b.style.display = "none";
        b.style.opacity = "0";
      });
      // Remove any hidden class from original elements
      tachiParaElements.forEach((p) => {
        p.element.classList.remove("tachi-original-hidden");
      });
      break;

    case "bilingual":
      // Show both
      blocks.forEach((b) => {
        b.style.display = "block";
        b.style.opacity = "1";
      });
      tachiParaElements.forEach((p) => {
        p.element.classList.remove("tachi-original-hidden");
      });
      break;

    case "translated":
      // Show translations, hide originals
      blocks.forEach((b) => {
        b.style.display = "block";
        b.style.opacity = "1";
      });
      tachiParaElements.forEach((p) => {
        p.element.classList.add("tachi-original-hidden");
      });
      break;
  }
}

// ── Set Translate Mode ───────────────────────────────────────────────────────

function setTranslateMode(mode) {
  if (!["original", "bilingual", "translated"].includes(mode)) return;
  tachiTranslateMode = mode;
  applyTranslateMode(mode);

  // Update floating button state
  if (tachiFloatingBtn) {
    updateFloatingBtnLabel(tachiFloatingBtn, mode);
  }
}

// ── Floating Toggle Button ───────────────────────────────────────────────────

function addFloatingToggle() {
  // Remove existing if any
  if (tachiFloatingBtn) {
    tachiFloatingBtn.remove();
  }

  const btn = document.createElement("div");
  btn.className = "tachi-float-toggle";
  btn.setAttribute("data-tachi-mode", tachiTranslateMode);
  updateFloatingBtnLabel(btn, tachiTranslateMode);

  // Click to cycle: original → bilingual → translated → original
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    const modes = ["original", "bilingual", "translated"];
    const currentIdx = modes.indexOf(tachiTranslateMode);
    const nextMode = modes[(currentIdx + 1) % modes.length];
    setTranslateMode(nextMode);

    // Notify background/side panel about mode change
    chrome.runtime.sendMessage({
      type: "translation_mode_changed",
      mode: nextMode,
    }).catch(() => {});
  });

  document.body.appendChild(btn);
  tachiFloatingBtn = btn;
}

function updateFloatingBtnLabel(btn, mode) {
  const labels = {
    original: "原文",
    bilingual: "双语",
    translated: "译文",
  };
  const icons = {
    original: "📖",
    bilingual: "🌐",
    translated: "🌍",
  };
  btn.innerHTML = `${icons[mode]} <span class="tachi-toggle-label">${labels[mode]}</span>`;
  btn.setAttribute("data-tachi-mode", mode);
}

// ── Cleanup ──────────────────────────────────────────────────────────────────

function cleanupTranslationUI() {
  document.querySelectorAll(".tachi-trans-block").forEach((el) => el.remove());
  tachiParaElements.forEach((p) => {
    p.element.classList.remove("tachi-original-hidden");
  });
  if (tachiFloatingBtn) {
    tachiFloatingBtn.remove();
    tachiFloatingBtn = null;
  }
}

function cleanupTranslation() {
  cleanupTranslationUI();
  if (tachiStyleEl) {
    tachiStyleEl.remove();
    tachiStyleEl = null;
  }
  tachiTranslationActive = false;
  tachiParaElements = [];
}

// ── Inject Styles ────────────────────────────────────────────────────────────

function injectStyles() {
  if (tachiStyleEl) return;

  const css = `
    /* ── Translation blocks ── */
    .tachi-trans-block {
      display: block;
      padding: 6px 10px;
      margin: 4px 0 8px 0;
      line-height: 1.6;
      color: #334155;
      background: #f8fafc;
      border-left: 3px solid #94a3b8;
      border-radius: 0 3px 3px 0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      word-break: break-word;
    }

    /* Original hidden state */
    .tachi-original-hidden {
      display: none !important;
    }

    /* ── Floating toggle button ── */
    .tachi-float-toggle {
      position: fixed;
      bottom: 24px;
      right: 24px;
      z-index: 2147483647;
      display: flex;
      align-items: center;
      gap: 4px;
      padding: 8px 14px;
      background: #4299e1;
      color: white;
      border: none;
      border-radius: 24px;
      font-size: 13px;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      font-weight: 500;
      cursor: pointer;
      box-shadow: 0 2px 12px rgba(66, 153, 225, 0.4);
      transition: all 0.2s ease;
      user-select: none;
    }
    .tachi-float-toggle:hover {
      background: #3182ce;
      box-shadow: 0 4px 16px rgba(66, 153, 225, 0.5);
      transform: translateY(-1px);
    }
    .tachi-float-toggle .tachi-toggle-label {
      font-size: 12px;
    }
    .tachi-float-toggle[data-tachi-mode="original"] {
      background: #718096;
      box-shadow: 0 2px 12px rgba(113, 128, 150, 0.4);
    }
    .tachi-float-toggle[data-tachi-mode="original"]:hover {
      background: #4a5568;
    }
    .tachi-float-toggle[data-tachi-mode="translated"] {
      background: #38a169;
      box-shadow: 0 2px 12px rgba(56, 161, 105, 0.4);
    }
    .tachi-float-toggle[data-tachi-mode="translated"]:hover {
      background: #2f855a;
    }

    /* ── Translation progress indicator ── */
    .tachi-trans-progress {
      position: fixed;
      bottom: 24px;
      right: 24px;
      z-index: 2147483647;
      display: flex;
      align-items: center;
      gap: 8px;
      padding: 10px 16px;
      background: #1a202c;
      color: #e2e8f0;
      border-radius: 8px;
      font-size: 13px;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      box-shadow: 0 2px 12px rgba(0, 0, 0, 0.3);
    }
    .tachi-trans-progress .spinner {
      width: 16px;
      height: 16px;
      border: 2px solid #4a5568;
      border-top-color: #4299e1;
      border-radius: 50%;
      animation: tachi-spin 0.8s linear infinite;
    }
    @keyframes tachi-spin {
      to { transform: rotate(360deg); }
    }
  `;

  tachiStyleEl = document.createElement("style");
  tachiStyleEl.id = "tachi-translation-styles";
  tachiStyleEl.textContent = css;
  document.head.appendChild(tachiStyleEl);
}

// ── Show/Hide Progress ───────────────────────────────────────────────────────

function showTranslationProgress() {
  const existing = document.querySelector(".tachi-trans-progress");
  if (existing) existing.remove();

  const el = document.createElement("div");
  el.className = "tachi-trans-progress";
  el.innerHTML = '<div class="spinner"></div> 正在翻译页面…';
  document.body.appendChild(el);
  return el;
}

function hideTranslationProgress() {
  const el = document.querySelector(".tachi-trans-progress");
  if (el) el.remove();
}
