// Package migrations embeds the versioned SQL migration files so the
// binary can apply them without a separate checkout.
//
// Files are applied in lexical order under an advisory lock by
// internal/database. Keep migrations to plain DDL: the statement
// splitter does not parse plpgsql bodies.
package migrations

import "embed"

// FS holds migrations/*.sql embedded at build time.
//
//go:embed *.sql
var FS embed.FS
