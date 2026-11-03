// Package exampleprotos embeds the example .proto files, so the example
// server (examples/testgrpcserver) serves exactly the schemas the example
// scenarios point at, rather than a copy that could drift.
package exampleprotos

import "embed"

// FS holds user.proto and feed.proto.
//
//go:embed *.proto
var FS embed.FS
