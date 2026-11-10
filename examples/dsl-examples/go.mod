module dsl-examples

go 1.27.3

require github.com/jecklgamis/resonate v0.0.0

require (
	github.com/HdrHistogram/hdrhistogram-go v1.4.0 // indirect
	github.com/PuerkitoBio/goquery v1.13.0 // indirect
	github.com/andybalholm/brotli v1.3.0 // indirect
	github.com/andybalholm/cascadia v1.3.4 // indirect
	github.com/antchfx/xmlquery v1.5.1 // indirect
	github.com/antchfx/xpath v1.3.8 // indirect
	github.com/bufbuild/protocompile v0.14.1 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/ohler55/ojg v1.28.6 // indirect
	github.com/pierrec/lz4/v4 v4.1.33 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/quic-go/quic-go v0.65.0 // indirect
	github.com/twmb/franz-go v1.22.2 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.15.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/grpc v1.85.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
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
