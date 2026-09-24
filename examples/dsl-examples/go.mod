module dsl-examples

go 1.27.1

require github.com/jecklgamis/resonate v0.0.0

require (
	github.com/antchfx/xmlquery v1.5.1 // indirect
	github.com/antchfx/xpath v1.3.8 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/ohler55/ojg v1.28.6 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// This example lives inside the resonate repo itself, so it points the
// dependency at the local checkout instead of a published version — this
// is also exactly what you'd do to try out an unreleased change before
// resonate has a tagged version: `replace github.com/jecklgamis/resonate
// => /path/to/your/local/checkout`. Once resonate has real releases, a
// standalone project would drop this line and just run
// `go get github.com/jecklgamis/resonate@latest`.
replace github.com/jecklgamis/resonate => ../..
