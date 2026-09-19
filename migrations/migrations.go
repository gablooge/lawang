// Package migrations embeds the SQL that defines Sluiceway's database.
package migrations

import "embed"

// FS holds the goose migrations, applied by "sluiceway migrate" as the application role.
//
//go:embed *.sql
var FS embed.FS

// Bootstrap is the one-time script an administrator runs before the first migration. It creates
// the three roles and the schema, which the non-superuser application role cannot do for itself.
//
//go:embed bootstrap/roles.sql
var Bootstrap string
