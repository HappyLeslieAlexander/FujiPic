// Package migrations embeds the SQL migration files so the binary can
// migrate a fresh database without relying on the working directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
