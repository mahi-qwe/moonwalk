// Small helpers shared by the customer and kitchen pages.

const POLL_MS = 5000; // how often we ask the server for fresh estimates

// The restaurant id is the part after /r/ in the URL.
const RESTAURANT = decodeURIComponent(location.pathname.split("/")[2] || "");
const API = `/api/restaurants/${encodeURIComponent(RESTAURANT)}`;

async function api(method, path, body) {
  const res = await fetch(API + path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `request failed (${res.status})`);
  return data;
}

// One banner for every error, hidden again once things work.
function showError(msg) {
  const b = document.getElementById("banner");
  b.textContent = msg;
  b.hidden = !msg;
}

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

// Deadlines use the server's remaining_seconds, so a wrong phone clock doesn't matter.
const deadlines = new Map(); // order id -> { at: ms timestamp, total: seconds }

function trackDeadlines(orders) {
  const now = Date.now();
  for (const o of orders) {
    const total = o.estimated_ready_at ? (Date.parse(o.estimated_ready_at) - Date.parse(o.created_at)) / 1000 : 0;
    deadlines.set(o.id, { at: now + o.remaining_seconds * 1000, total });
  }
}

function secondsLeft(id) {
  const d = deadlines.get(id);
  return d ? Math.max(0, Math.round((d.at - Date.now()) / 1000)) : 0;
}

function mmss(sec) {
  const m = Math.floor(sec / 60), s = sec % 60;
  return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

function itemsText(items) {
  return items.map((it) => `${it.qty}× ${it.name}`).join(", ");
}

// Updates every [data-clock] once a second, between polls.
function tickClocks() {
  for (const el of document.querySelectorAll("[data-clock]")) {
    const id = Number(el.dataset.clock);
    const left = secondsLeft(id);
    el.textContent = left > 0 ? mmss(left) : "any moment";
    const bar = document.querySelector(`[data-bar="${id}"]`);
    const d = deadlines.get(id);
    if (bar && d && d.total > 0) bar.style.width = `${Math.min(100, 100 * (1 - left / d.total))}%`;
  }
}
setInterval(tickClocks, 1000);
