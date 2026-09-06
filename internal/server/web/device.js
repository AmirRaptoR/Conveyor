import { $ } from "./dom.js";
import { state, fault } from "./shared.js";
import { inspect } from "./panel.js";

// ---- the app on a device -------------------------------------------------
// Installing is the browser's gesture; this only registers the worker that
// makes the page installable and receives a push. A subscription is taken on
// the first press of the button, again silently whenever permission is
// already granted (an install, a reinstall, a cleared server), and posted on
// every load so the server always holds the current one.
const notifyBtn = $("#notify");
const b64ToU8 = s => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), c => c.charCodeAt(0));

async function pushState() {
  if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) return null;
  const reg = await navigator.serviceWorker.ready;
  return { reg, sub: await reg.pushManager.getSubscription() };
}
async function subscribePush() {
  const st = await pushState();
  if (!st) return;
  notifyBtn.disabled = true;
  try {
    if ((await Notification.requestPermission()) !== "granted") return;
    const { key } = await (await fetch("/api/push/key")).json();
    if (!key) { alert("The server has no push key; notifications are off there."); return; }
    const sub = await st.reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToU8(key) });
    await fetch("/api/push/subscribe", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(sub) });
  } catch (err) {
    alert(`Could not turn notifications on: ${err.message || err}`);
  } finally {
    notifyBtn.disabled = false;
    drawNotify();
  }
}
async function unsubscribePush() {
  const st = await pushState();
  if (!st || !st.sub) return;
  if (!confirm("Turn notifications off on this device?")) return;
  await fetch("/api/push/unsubscribe", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ endpoint: st.sub.endpoint }) });
  await st.sub.unsubscribe();
  drawNotify();
}
async function drawNotify() {
  const st = await pushState();
  if (!st) { notifyBtn.hidden = true; return; }
  notifyBtn.hidden = false;
  if (Notification.permission === "denied") {
    notifyBtn.textContent = "Notifications blocked";
    notifyBtn.title = "Allow notifications for this site in the browser's settings";
    notifyBtn.onclick = null;
    return;
  }
  notifyBtn.textContent = st.sub ? "Notifications on" : "Notify me";
  notifyBtn.title = st.sub ? "Press to turn them off on this device" : "Get a push when something needs you or ships";
  notifyBtn.onclick = st.sub ? unsubscribePush : subscribePush;
  if (st.sub) fetch("/api/push/subscribe", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(st.sub) });
}
if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("/sw.js").then(async () => {
    await drawNotify();
    const st = await pushState();
    if (!st || st.sub) return;
    const installed = matchMedia("(display-mode: standalone)").matches || navigator.standalone;
    // Already allowed, or running as the installed app: subscribe without a press.
    if (Notification.permission === "granted" || (installed && Notification.permission === "default")) subscribePush();
  }).catch(() => {});
  addEventListener("appinstalled", () => subscribePush());
}

// A notification opens the item it was about.
export function openFromHash() {
  const m = location.hash.match(/^#item=(.+)$/);
  if (!m || !state) return;
  const id = decodeURIComponent(m[1]);
  const it = (state.items || []).find(i => i.id === id);
  history.replaceState(null, "", location.pathname);
  if (it) inspect(it.id, it.title, it.stage);
}
addEventListener("hashchange", openFromHash);
$("#tick").onclick = async e => {
  const btn = e.target;
  btn.disabled = true;
  let res;
  try {
    res = await fetch("/api/tick", { method:"POST" });
  } catch {
    btn.disabled = false;
    fault("could not tick: disconnected");
    return;
  }
  if (res.status === 409) { $("#line1").textContent = "a tick is already in flight"; }
  else if (!res.ok) {
    btn.disabled = false;
    fault((await res.text()).trim() || `could not tick (HTTP ${res.status})`);
    return;
  }
  setTimeout(() => btn.disabled = false, 1500);
};
