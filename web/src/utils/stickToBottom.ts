// Stick-to-bottom auto-scroll for live logs/chats: follow new content only
// while the reader is already at the bottom. Once the user scrolls up, the
// view must stay put — heartbeat/log updates must not yank the viewport back
// down. Stick state is tracked from the user's scroll events, NOT derived at
// update time: after content grows, the distance-from-bottom includes the new
// content and would misclassify a reader who was at the bottom.
export const STICK_THRESHOLD_PX = 80

export function isAtBottom(el: HTMLElement, threshold = STICK_THRESHOLD_PX): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= threshold
}

export function stickToBottom(el: HTMLElement): void {
  el.scrollTop = el.scrollHeight
}
