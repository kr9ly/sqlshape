package postgres

import (
	"context"
	"strings"
)

// MatView is a handle on a materialized view; the checker verifies the name against
// schema.sql (and, with -strict, that a unique index allows RefreshConcurrently).
//
//	var OrderStats = postgres.MatView("order_stats")
//	err := OrderStats.Refresh(ctx, db)
type MatView string

// Refresh runs REFRESH MATERIALIZED VIEW: readers block until it completes.
func (m MatView) Refresh(ctx context.Context, db DB) error {
	_, err := db.Exec(ctx, "REFRESH MATERIALIZED VIEW "+quoteQualified(string(m)))
	return err
}

// RefreshConcurrently refreshes without blocking readers; the view needs a unique index.
func (m MatView) RefreshConcurrently(ctx context.Context, db DB) error {
	_, err := db.Exec(ctx, "REFRESH MATERIALIZED VIEW CONCURRENTLY "+quoteQualified(string(m)))
	return err
}

// quoteQualified quotes a possibly schema-qualified identifier.
func quoteQualified(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}
