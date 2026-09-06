// The two helpers every other module needs, in a module of their own so it
// is evaluated before any of them: a module that reached for `$` while the
// module declaring it was still evaluating would get a TDZ error, and the
// board's modules import each other in cycles (see board.js/drag.js).

export const $ = s => document.querySelector(s);
export const esc = s => (s ?? "").replace(/[&<>"]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));
