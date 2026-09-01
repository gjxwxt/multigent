import type { KeyboardEvent } from 'react'

/**
 * True while an IME composition session is active (e.g. a Chinese input
 * method is picking candidates). Enter keydowns during composition only
 * confirm the selected text into the field and must not trigger submit or
 * send actions. keyCode 229 covers browsers that report isComposing=false
 * on the confirming keydown itself.
 */
export function isImeComposing(e: KeyboardEvent): boolean {
  return e.nativeEvent.isComposing || e.keyCode === 229
}
