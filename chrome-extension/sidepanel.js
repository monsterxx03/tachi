// sidepanel.js — Tachi Side Panel
//
// Note: Must be loaded via <script src="sidepanel.js"></script> (not inline)
// because Chrome CSP blocks inline scripts in MV3 side panels.

console.log("Tachi side panel: script loaded");

const container = document.getElementById("chatContainer");
const input = document.getElementById("input");
const sendBtn = document.getElementById("sendBtn");

if (!container || !input || !sendBtn) {
  console.error("Tachi side panel: missing DOM elements", { container, input, sendBtn });
} else {
  // Auto-resize textarea
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 120) + "px";
  });

  // Send on Enter
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });

  sendBtn.addEventListener("click", send);
}

async function send() {
  const text = input.value.trim();
  if (!text) return;

  addMessage("user", text);
  input.value = "";
  input.style.height = "auto";

  try {
    const response = await chrome.runtime.sendMessage({
      type: "sidepanel_query",
      content: text,
    });
    console.log("Tachi side panel: got response", response);
    if (response && response.content) {
      addMessage("assistant", response.content);
    }
  } catch (e) {
    console.error("Tachi side panel: sendMessage error", e);
    addMessage("assistant", `❌ ${e.message}`);
  }
}

function addMessage(role, content) {
  console.log("Tachi side panel: addMessage", role, content.substring(0, 50));
  if (!container) return;

  // Remove empty state
  const empty = container.querySelector(".empty-state");
  if (empty) empty.remove();

  const msg = document.createElement("div");
  msg.className = `message ${role}`;
  msg.textContent = content;
  container.appendChild(msg);
  msg.scrollIntoView({ behavior: "smooth" });
}

const ACTION_TITLES = {
  "ask_tachi": "🤖 问 Tachi",
  "explain":   "📖 解释",
  "search":    "🔍 搜索",
  "remember":  "🧠 记住",
  "recall":    "🔎 回忆",
};

// Long-lived port to background service worker
const bgPort = chrome.runtime.connect({ name: "tachi-sidepanel" });
bgPort.onMessage.addListener((msg) => {
  console.log("Tachi side panel: port message", msg);
  if (msg.type === "show_result") {
    const actionTitle = ACTION_TITLES[msg.action] || "🤖 Tachi";
    addMessage("assistant", `${actionTitle}\n\n${msg.content}`);
  }
  if (msg.type === "show_error") {
    addMessage("assistant", `⚠️ ${msg.content}`);
  }
});

// Also listen via runtime.onMessage as backup
chrome.runtime.onMessage.addListener((msg) => {
  console.log("Tachi side panel: runtime message", msg);
  if (msg.type === "show_result") {
    const actionTitle = ACTION_TITLES[msg.action] || "🤖 Tachi";
    addMessage("assistant", `${actionTitle}\n\n${msg.content}`);
  }
  if (msg.type === "show_error") {
    addMessage("assistant", `⚠️ ${msg.content}`);
  }
});

console.log("Tachi side panel: init complete");
