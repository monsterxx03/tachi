// translate-popup.js — Tachi Translation Popup
//
// Displays the result of a "translate selection" request.
// Reads request ID from URL and fetches the result from background.

document.addEventListener("DOMContentLoaded", async () => {
  const content = document.getElementById("content");
  const loading = document.getElementById("loading");

  if (!content || !loading) return;

  // Read request ID from URL query parameter
  const params = new URLSearchParams(window.location.search);
  const requestId = parseInt(params.get("id"), 10);

  try {
    const response = await chrome.runtime.sendMessage({
      type: "get_latest_translation",
      requestId: requestId,
    });

    loading.style.display = "none";

    if (response && response.error) {
      const err = document.createElement("div");
      err.className = "error";
      err.textContent = `翻译失败: ${response.error}`;
      content.appendChild(err);
      return;
    }

    if (response && response.translation) {
      // Original text
      if (response.original) {
        const label = document.createElement("div");
        label.className = "section-label";
        label.textContent = "原文";
        content.appendChild(label);

        const orig = document.createElement("div");
        orig.className = "original-text";
        orig.textContent = response.original;
        content.appendChild(orig);
      }

      // Translation
      const label = document.createElement("div");
      label.className = "section-label";
      label.textContent = "译文";
      content.appendChild(label);

      const trans = document.createElement("div");
      trans.className = "translation-text";
      trans.textContent = response.translation;
      content.appendChild(trans);
    } else {
      const err = document.createElement("div");
      err.className = "error";
      err.textContent = "翻译返回了空结果";
      content.appendChild(err);
    }
  } catch (err) {
    loading.style.display = "none";
    const errDiv = document.createElement("div");
    errDiv.className = "error";
    errDiv.textContent = `通信失败: ${err.message}`;
    content.appendChild(errDiv);
  }
});
