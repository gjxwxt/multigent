export type DOMTarget = {
  tag: string
  tagName: string
  id?: string
  role?: string
  type?: string
  testId?: string
  selector: string
}

export const SAFE_TAG_REGEX = /^[a-z][a-z0-9-]{0,31}$/
export const SAFE_ID_REGEX = /^[A-Za-z][A-Za-z0-9_:-]{0,63}$/
export const SAFE_ROLE_TYPE_REGEX = /^[a-zA-Z0-9_\-]{1,32}$/
export const SAFE_SELECTOR_SEGMENT_REGEX = /^[a-z][a-z0-9-]{0,31}(?::nth-of-type\(\d+\))?$/

export function decodeDOMTarget(raw: unknown): DOMTarget | null {
  if (!raw || typeof raw !== 'object') return null
  const obj = raw as Record<string, unknown>

  // 1. Authoritative tagName extraction & validation
  // Extract tagName (or fallback to tag), must be string and match SAFE_TAG_REGEX
  const rawTagName = typeof obj.tagName === 'string' && obj.tagName.trim()
    ? obj.tagName.trim().toLowerCase()
    : (typeof obj.tag === 'string' ? obj.tag.trim().toLowerCase() : '')

  if (!SAFE_TAG_REGEX.test(rawTagName)) {
    return null
  }

  // If a separate obj.tag was provided, it must also be a pure safe tag name.
  // Forged strings containing '#' or '.' (e.g. "button#secret") fail-closed reject the message.
  if (typeof obj.tag === 'string') {
    const rawTag = obj.tag.trim().toLowerCase()
    if (!SAFE_TAG_REGEX.test(rawTag)) {
      return null
    }
  }

  // 2. Validate selector: non-empty sequence of segments separated by " > ", each matching SAFE_SELECTOR_SEGMENT_REGEX
  const selector = typeof obj.selector === 'string' ? obj.selector.trim() : ''
  if (!selector) return null
  const segments = selector.split(' > ')
  if (segments.length === 0 || segments.length > 20) return null
  for (const seg of segments) {
    if (!SAFE_SELECTOR_SEGMENT_REGEX.test(seg)) {
      return null
    }
  }

  // 3. Parent takes validated tagName as authoritative sole basis for tag
  const result: DOMTarget = {
    tag: rawTagName,
    tagName: rawTagName,
    selector,
  }

  if (typeof obj.id === 'string' && obj.id.trim()) {
    const cleanId = obj.id.trim()
    if (SAFE_ID_REGEX.test(cleanId)) {
      result.id = cleanId
    }
  }

  if (typeof obj.role === 'string' && obj.role.trim()) {
    const cleanRole = obj.role.trim()
    if (SAFE_ROLE_TYPE_REGEX.test(cleanRole)) {
      result.role = cleanRole
    }
  }

  if (typeof obj.type === 'string' && obj.type.trim()) {
    const cleanType = obj.type.trim()
    if (SAFE_ROLE_TYPE_REGEX.test(cleanType)) {
      result.type = cleanType
    }
  }

  if (typeof obj.testId === 'string' && obj.testId.trim()) {
    const cleanTestId = obj.testId.trim()
    if (SAFE_ROLE_TYPE_REGEX.test(cleanTestId)) {
      result.testId = cleanTestId
    }
  }

  return result
}
