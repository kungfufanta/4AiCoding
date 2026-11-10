// Package migrations embeds the Portal SQL migrations so the single
// portal binary can migrate its own database at boot (like sandboxd
// does for its SQLite store). Files must be named NNNN_name.sql and
// are applied in numeric order inside one transaction each.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
