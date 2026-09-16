package changerun

import "fmt"

// NotFoundError marks a lookup failure the HTTP layer must render as 404 —
// notably scope violations: a proposal ID that exists but does not belong to
// the URL's (project, task) must be indistinguishable from a missing one
// (round-18 P0-1).
type NotFoundError struct {
	Kind string // "proposal"
	ID   string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %s not found", e.Kind, e.ID)
}
