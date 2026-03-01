package hasspoller

import _ "embed"

// SchemaSQL contains the startup database schema.
//
//go:embed schema.sql
var SchemaSQL string
