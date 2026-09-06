// The board's entry module: the one script index.html loads, and the only
// module with side effects of its own beyond wiring its own handlers — the
// live event stream, the duration ticker, and the first poll.
import { $ } from "./dom.js";
import { load, loadDoctor, fault } from "./shared.js";
import { tickDurations } from "./board.js";
import { openItemId, followRun, logPending, logBuffer, loadHistory, renderLine, trimLog } from "./panel.js";

const es = new EventSource("/api/events");
es.onmessage = ev => {
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
  load();
  loadDoctor();
};
es.onerror = () => fault("disconnected");

setInterval(() => { if (!document.hidden) tickDurations(); }, 1000);
// A tab backgrounded for an hour and brought back must read correctly at
// once, not resume ticking from whatever it last showed — so this runs
// unconditionally, the one call that ignores document.hidden on purpose.
addEventListener("visibilitychange", () => { if (!document.hidden) tickDurations(); });

load();
loadDoctor();
