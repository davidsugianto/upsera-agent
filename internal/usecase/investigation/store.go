// Package investigation orchestrates a diagnosis: it collects evidence through
// the registered read-only tools, stores each call, and runs the detectors.
package investigation

import (
	"context"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// EvidenceStore persists evidence rows (ADR 0004). Only redacted excerpts are
// ever passed to it.
type EvidenceStore interface {
	Add(ctx context.Context, e domain.Evidence) error
	List(ctx context.Context, investigationID string) ([]domain.Evidence, error)
	Close() error
}
