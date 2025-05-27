const ws = new WebSocket("ws://localhost:8080/ws");
const chatHistory = document.getElementById("chatHistory");
const queryInput = document.getElementById("queryInput");

ws.onmessage = function (event) {
  const msg = JSON.parse(event.data);
  appendMessage(msg);
  chatHistory.scrollTop = chatHistory.scrollHeight;
};

ws.onerror = function (error) {
  console.error("WebSocket error:", error);
};

ws.onclose = function () {
  appendMessage({
    role: "bot",
    content: "Connection closed. Please refresh the page.",
    time: new Date().toLocaleTimeString(),
  });
};

function sendQuery() {
  const query = queryInput.value.trim();
  if (!query) return;
  ws.send(query);
  queryInput.value = "";
}

queryInput.addEventListener("keypress", function (e) {
  if (e.key === "Enter") {
    sendQuery();
  }
});

function appendMessage(msg) {
  const div = document.createElement("div");
  div.className = `message ${msg.role}`;
  div.innerHTML = `
        <div class="time">${msg.time}</div>
        <div>${msg.content.replace(/\n/g, "<br>")}</div>
    `;
  chatHistory.appendChild(div);
}
