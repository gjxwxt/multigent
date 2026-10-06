---
name: a11y-debugging
description: Accessibility (a11y) auditing and remediation based on WCAG 2.1 AA guidelines, semantic HTML, ARIA, and focus states.
---

# Skill: Accessibility (a11y) Remediation

When auditing and improving web interface accessibility, adhere to WCAG 2.1 Level AA compliance:

## 1. Semantic HTML & Landmarks
- Use semantic structure elements (`<header>`, `<nav>`, `<main>`, `<article>`, `<section>`, `<footer>`) instead of generic `<div>` soup.
- Use native button elements (`<button>`) for clickable actions and anchor elements (`<a>`) for navigations.
- Provide descriptive `alt` text for informative images, and empty `alt=""` for decorative icons.

## 2. Keyboard Navigation & Focus Management
- Ensure all interactive elements are reachable via `Tab` key and have visible `:focus-visible` focus indicators.
- Do not remove outline styles (`outline: none`) without providing an equivalent, high-contrast visual focus ring.
- Ensure logical tab order matching the visual layout. Avoid positive `tabindex` values.
- Trap focus inside active modal dialogs and restore focus to the triggering element upon closure.

## 3. ARIA & Color Contrast
- Use ARIA attributes (`aria-expanded`, `aria-haspopup`, `aria-controls`, `aria-label`, `aria-describedby`) only when native HTML semantics are insufficient (First Rule of ARIA).
- Maintain minimum contrast ratio of 4.5:1 for normal text and 3:1 for large text (18pt or 14pt bold) against background colors in both light and dark modes.
- Never convey critical information using color alone; pair colors with icons, labels, or patterns.
