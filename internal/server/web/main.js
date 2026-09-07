// The board's entry module: the one script index.html loads, and the only
// module with side effects of its own beyond wiring its own handlers — the
// live event stream, the duration ticker, and the first poll.
import { $ } from "./dom.js";
import { load, loadDoctor, fault } from "./shared.js";
import { tickDurations } from "./board.js";
import { openItemId, followRun, logPending, logBuffer, loadHistory, renderLine, trimLog } from "./panel.js";

// How the board finds out anything changed. The stream is the fast path and
// the poll is the one that is always right — the stream is a live TCP
// connection, and a phone that locks its screen, a laptop that suspends, a
// proxy with an idle timeout and a carrier switching networks all end it
// without the page being told anything useful. The board used to redraw only
// from a stream message, so any of those left it showing a snapshot from
// whenever the connection died, indefinitely, until someone reloaded.
const SLOW_POLL = 20000;

let es = null;
let retry = 0;

function onMessage(ev) {
  const e = JSON.parse(ev.data);
  if (e.kind === "log") {
    if (e.runId !== followRun || !e.line) return;
    // The snapshot for this run has not rendered yet — buffer rather than
    // append, or a line can land above lines the snapshot has not shown yet.
    if (logPending) { logBuffer.push(e.line); return; }
    const log = $("#log");
    const pinned = log.scrollHeight - log.scrollTop - log.clientHeight < 48;
    log.appendChild(renderLine(e.line));
    trimLog(log);
    if (pinned) log.scrollTop = log.scrollHeight;
    return;
  }
  if (e.kind === "transition" && openItemId && e.transition?.item?.id === openItemId) loadHistory(openItemId);
  refresh();
}

// EventSource reconnects on its own, but only while the page is running and
// only from errors it saw; a stream torn down while the tab was frozen comes
// back in whatever state the browser left it, and on iOS commonly never comes
// back at all. Closing and reopening ours is the one thing that is reliable,
// so connect() is safe to call whenever the board is in doubt.
function connect() {
  if (es) es.close();
  es = new EventSource("/api/events");
  es.onmessage = onMessage;
  es.onopen = () => { retry = 0; refresh(); };
  es.onerror = () => {
    fault("reconnecting…");
    // Backed off, because a server that is down stays down for a while and a
    // phone reconnecting every second is a phone with a flat battery. The
    // slow poll keeps running throughout, so the board is still correct
    // while the stream is not.
    retry = Math.min(retry + 1, 5);
    if (es) { es.close(); es = null; }
    setTimeout(() => { if (!es) connect(); }, retry * 2000);
  };
}

function refresh() { load(); loadDoctor(); }

// Awake and visible: poll regardless of the stream. Hidden tabs cost nothing
// and learn what they missed the moment they come back.
setInterval(() => { if (!document.hidden) refresh(); }, SLOW_POLL);
setInterval(() => { if (!document.hidden) tickDurations(); }, 1000);

// Coming back from a locked phone or a background tab. tickDurations runs
// unconditionally so the ages read correctly at once rather than resuming from
// whatever they last showed; refresh and connect run because the stream is the
// thing most likely to have died while nobody was looking.
addEventListener("visibilitychange", () => {
  if (document.hidden) return;
  tickDurations();
  refresh();
  if (!es || es.readyState === EventSource.CLOSED) connect();
});
addEventListener("online", () => { refresh(); connect(); });
addEventListener("pageshow", () => { refresh(); connect(); });

connect();
refresh();
