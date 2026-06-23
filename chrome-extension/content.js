// content.js — Tachi Chrome Extension Content Script
//
// Injected into every page. Provides page content extraction using
// Mozilla's Readability for clean article text extraction.

import { Readability } from "@mozilla/readability";

// ── Message Handler ──────────────────────────────────────────────────────────

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg.type === "get_page_content") {
    const result = extractPageContent();
    sendResponse(result);
    return true; // keep channel open for async? No, this is sync. But return value indicates we'll respond.
  }
  return false;
});

// ── Content Extraction ───────────────────────────────────────────────────────

const MAX_CONTENT_LENGTH = 100_000; // ~25K tokens, well within 1MB Native Messaging limit

function extractPageContent() {
  try {
    // Clone the document to avoid mutating the live page
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
    console.error("Tachi: Readability extraction failed, falling back to body text:", e.message);
  }

  // Fallback: use document.body.innerText
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
  // Truncate at a sentence/paragraph boundary if possible
  const truncated = text.substring(0, maxLength);
  const lastPeriod = truncated.lastIndexOf("。");
  const lastNewline = truncated.lastIndexOf("\n\n");
  const cutPoint = Math.max(lastPeriod, lastNewline);
  if (cutPoint > maxLength * 0.7) {
    return truncated.substring(0, cutPoint + 1) + "\n\n[内容已截断…]";
  }
  return truncated + "\n\n[内容已截断…]";
}
