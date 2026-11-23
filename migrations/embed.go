// Package migrations embeds the SQL schema applied by the installer connection.
package migrations

import "embed"

// Files contains the ordered *.sql migrations. Names sort lexicographically.
//
//go:embed *.sql
var Files embed.FS
