---
name: modern-web-guidance
description: Modern web development guidance for semantic CSS layout, fluid typography, container queries, and responsive components.
---

# Skill: Modern Web Guidance

When refining frontend user interfaces, adhere to modern Web standards and best practices:

## 1. Modern CSS Layout & Styling
- Prefer CSS Flexbox and Grid over floats or absolute positioning hacks.
- Utilize CSS container queries (`@container`) for component-level responsiveness rather than solely relying on global media queries.
- Use modern CSS selectors like `:has()`, `:is()`, and `:where()` to minimize unnecessary wrapper elements or redundant classes.
- Ensure proper use of CSS custom properties (variables) for consistent colors, margins, and typography across themes (light and dark mode).

## 2. Responsive & Fluid Design
- Use fluid typography with `clamp()` for smooth scaling across mobile, tablet, and desktop viewports.
- Design mobile-first or ensure responsive layouts adapt naturally without horizontal scrollbars on viewports down to 320px width.
- Use `touch-action`, adequate tap target sizes (minimum 44x44 CSS pixels), and responsive hit-boxes for interactive elements.

## 3. Form & Component Interaction
- Build forms with native HTML input types (`type="email"`, `type="url"`, `type="number"`, `type="date"`) and valid attributes (`required`, `autocomplete`, `pattern`).
- Provide immediate, accessible visual feedback for form states (`:hover`, `:focus-visible`, `:disabled`, `:invalid`).
- Ensure all custom popovers, dropdowns, and modals handle backdrop clicks and escape key dismissals cleanly.
