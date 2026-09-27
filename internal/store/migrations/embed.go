// Package migrations embeds abusekit's SQL migrations so the binary can
// self-apply them at startup (design §4.11: "Migrations embedded,
// expand-only").
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
