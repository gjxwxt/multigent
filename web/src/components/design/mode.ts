// Design gate embedding mode, fixed by the Phase 0 verdict
// (docs/opendesign-integration-plan.md appendix A): the OD Studio UI renders
// fully inside a cross-origin iframe through the design proxy, so Plan A is
// the default. Plan B ('external') stays available as a runtime fallback —
// flip this constant if a future OD build introduces frame-ancestors CSP or
// breaks the rewrite layer.
export const DESIGN_EMBED_MODE: 'iframe' | 'external' = 'iframe'
