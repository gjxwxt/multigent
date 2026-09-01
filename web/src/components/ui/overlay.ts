import type { MouseEvent } from 'react'

// Overlays whose press started inside the dialog must not dismiss when the
// release lands on the backdrop: the browser fires `click` on the nearest
// common ancestor of the mousedown/mouseup targets, which is the overlay
// itself. Tracking the press location distinguishes a real backdrop click
// from a text-selection drag that ends outside the dialog.
const pressedOnOverlay = new WeakSet<EventTarget>()

/**
 * Props for a modal backdrop container. Spread onto the overlay element in
 * place of a plain `onClick={dismiss}`: dismissal then requires both the
 * mouse press and the release to land on the overlay itself.
 */
export function overlayDismissProps(dismiss: () => void) {
  return {
    onMouseDown: (e: MouseEvent<HTMLDivElement>) => {
      if (e.target === e.currentTarget) pressedOnOverlay.add(e.currentTarget)
      else pressedOnOverlay.delete(e.currentTarget)
    },
    onClick: (e: MouseEvent<HTMLDivElement>) => {
      const pressed = pressedOnOverlay.delete(e.currentTarget)
      if (pressed && e.target === e.currentTarget) dismiss()
    },
  }
}
