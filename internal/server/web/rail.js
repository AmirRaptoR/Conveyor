import { $ } from "./dom.js";

// The rail's own horizontal scrollbar lives at its bottom edge, which on a
// busy board is off the bottom of the viewport — see #15. This control is
// pinned in the rail's top padding instead, so it is in view from the top of
// the page. It lives outside `.line` so re-rendering `#rail` (every state
// update) never touches it and never resets `.line`'s scrollLeft.
function lineMax() {
  const line = $("#line");
  return Math.max(line.scrollWidth - line.clientWidth, 0);
}

export function updateRailCtl() {
  const line = $("#line"), ctl = $("#railctl"), track = $("#rail-track");
  // .line is the scroll container — its own scrollWidth includes its padding,
  // which is what scrollLeft is measured against. #rail's scrollWidth is the
  // flex row alone and undercounts that padding, so max would be reached
  // before the true end of the range.
  const overflow = line.scrollWidth > line.clientWidth + 1;
  ctl.hidden = !overflow;
  if (!overflow) return;
  const max = lineMax();
  const ratio = Math.max(line.clientWidth / line.scrollWidth, 0.06);
  $("#rail-thumb").style.width = `${ratio * 100}%`;
  $("#rail-thumb").style.left = `${(line.scrollLeft / max) * (1 - ratio) * 100}%`;
  $("#rail-left").disabled = line.scrollLeft <= 0;
  $("#rail-right").disabled = line.scrollLeft >= max - 1;
  track.setAttribute("aria-valuemax", String(Math.round(max)));
  track.setAttribute("aria-valuenow", String(Math.round(line.scrollLeft)));
}

function scrollRail(dir) {
  const line = $("#line");
  const max = lineMax();
  const step = Math.max(line.clientWidth * 0.8, 120);
  line.scrollLeft = Math.min(Math.max(line.scrollLeft + dir * step, 0), max);
  updateRailCtl();
}

// --- rail drag/click/wheel/keyboard -----------------------------------------
// The pinned control behaves like a real scrollbar: drag the thumb, click the
// track to jump, wheel over it, arrow keys when it has focus. Geometry is
// measured against `.line` (see updateRailCtl's comment above) so a drag
// never falls short of the true end.
function railGeometry() {
  const track = $("#rail-track");
  const trackRect = track.getBoundingClientRect();
  const line = $("#line");
  const ratio = Math.max(line.clientWidth / line.scrollWidth, 0.06);
  const thumbW = ratio * trackRect.width;
  return { trackRect, thumbW, travel: trackRect.width - thumbW, max: lineMax() };
}

function railScrollTo(t) {
  const { max } = railGeometry();
  $("#line").scrollLeft = Math.round(Math.min(Math.max(t, 0), 1) * max);
  updateRailCtl();
}

let railDrag = null;
function railTrackDown(e) {
  if (e.button !== undefined && e.button !== 0) return;
  const g = railGeometry();
  const clickX = e.clientX - g.trackRect.left;
  const thumbLeft = (g.max > 0 ? $("#line").scrollLeft / g.max : 0) * g.travel;
  const onThumb = clickX >= thumbLeft && clickX <= thumbLeft + g.thumbW;
  // Pointer travel maps to content travel by scrollableWidth / trackWidth
  // (g.max / g.travel), not 1:1 — grabbing the thumb preserves the offset
  // between the pointer and the thumb's own left edge, so it does not jump.
  const grabOffset = onThumb ? clickX - thumbLeft : g.thumbW / 2;
  railDrag = { pointerId: e.pointerId, grabOffset, travel: g.travel };
  $("#rail-track").setPointerCapture?.(e.pointerId);
  railScrollTo(g.travel > 0 ? (clickX - grabOffset) / g.travel : 0);
  e.preventDefault();
}
function railTrackMove(e) {
  if (!railDrag || e.pointerId !== railDrag.pointerId) return;
  const g = railGeometry();
  const x = e.clientX - g.trackRect.left - railDrag.grabOffset;
  railScrollTo(g.travel > 0 ? x / g.travel : 0);
}
function railTrackUp(e) {
  if (!railDrag || (e && e.pointerId !== undefined && e.pointerId !== railDrag.pointerId)) return;
  railDrag = null;
}
const railTrackEl = () => $("#rail-track");
railTrackEl().addEventListener("pointerdown", railTrackDown);
railTrackEl().addEventListener("pointermove", railTrackMove);
railTrackEl().addEventListener("pointerup", railTrackUp);
railTrackEl().addEventListener("pointercancel", railTrackUp);
railTrackEl().addEventListener("lostpointercapture", railTrackUp);
// A drag keeps tracking the pointer past the track's own box — above, below,
// or outside the window — because it is captured, not because it is still
// inside the element; these two cover a mouseup while the OS still has the
// button down but the tab lost focus (alt-tab mid-drag).
addEventListener("blur", () => { railDrag = null; });
addEventListener("visibilitychange", () => { if (document.hidden) railDrag = null; });

railTrackEl().addEventListener("keydown", e => {
  if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
  e.preventDefault();
  const line = $("#line"), max = lineMax();
  const step = Math.max(line.clientWidth * 0.1, 24);
  if (e.key === "ArrowLeft") line.scrollLeft = Math.max(line.scrollLeft - step, 0);
  else if (e.key === "ArrowRight") line.scrollLeft = Math.min(line.scrollLeft + step, max);
  else if (e.key === "Home") line.scrollLeft = 0;
  else if (e.key === "End") line.scrollLeft = max;
  updateRailCtl();
});

// Normalise a wheel notch across deltaMode 0 (pixels), 1 (lines), 2 (pages) to
// a roughly comparable pixel distance, so one notch of a normal mouse wheel
// moves the rail by a similar amount in each. Page mode is rare — the few
// devices that report it send one page (deltaY 1) per notch, and a "page" of
// the rail is easily thousands of pixels wide, so scaling by the rail's own
// width would move it by an entire screen for the same physical gesture that
// moves a handful of lines under mode 1. 120px keeps it on the same order as
// a mode-0 notch instead.
function wheelPixels(e) {
  if (e.deltaMode === 1) return e.deltaY * 16;
  if (e.deltaMode === 2) return e.deltaY * 120;
  return e.deltaY;
}

// A plain vertical wheel over the rail (or the pinned control) scrolls it
// horizontally while it has range left in that direction; at either end the
// event is left alone so the page scrolls instead — the board never swallows
// vertical scrolling at the ends of its range (#15). Only consumed when it
// actually moves the rail, which is what makes that release work and is why
// this cannot be a passive listener. Shift+wheel and a diagonal trackpad
// gesture are left to the browser entirely, so the two never double-apply.
function railWheel(e) {
  if (e.shiftKey) return;
  if (Math.abs(e.deltaX) > Math.abs(e.deltaY)) return;
  const line = $("#line"), max = lineMax();
  if (max <= 0) return;
  const px = wheelPixels(e);
  if (px < 0 && line.scrollLeft <= 0) return;
  if (px > 0 && line.scrollLeft >= max - 0.5) return;
  e.preventDefault();
  line.scrollLeft = Math.min(Math.max(line.scrollLeft + px, 0), max);
  updateRailCtl();
}
$("#line").addEventListener("wheel", railWheel, { passive: false });
$("#railctl").addEventListener("wheel", railWheel, { passive: false });

// Tracks a geometry change that fires no `resize` event at all: browser zoom,
// a font loading late, or `--station` changing at the 640px media query.
// `.line` is `overflow-x:auto`, so its own box does not move when only its
// content's width changes — a font finishing load after the first render
// grows `#rail`'s scrollWidth without touching `.line`'s box at all, so both
// are observed: `#rail` for content-width changes, `.line` for the container
// itself (zoom, `--station`).
const railResize = new ResizeObserver(updateRailCtl);
railResize.observe($("#line"));
railResize.observe($("#rail"));

$("#rail-left").onclick = () => scrollRail(-1);
$("#rail-right").onclick = () => scrollRail(1);
$("#line").addEventListener("scroll", updateRailCtl);
addEventListener("resize", updateRailCtl);
