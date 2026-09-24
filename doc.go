// Package resonate is the public, embeddable API for resonate's load
// generation engine — the same engine the resonate CLI (cmd/resonate) is
// built on. Import this package to drive load tests programmatically from
// your own Go program instead of shelling out to the CLI or writing a
// scenario YAML file.
//
// A minimal HTTP load test looks like:
//
//	gen, err := resonate.NewHTTPGenerator(
//		[]resonate.HTTPTarget{{URL: "http://localhost:8080/health"}},
//		resonate.DefaultHTTPOptions(),
//	)
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer gen.Close()
//
//	summary := resonate.Run(ctx, gen, resonate.Options{Rate: 50, Workers: 20, Duration: 30 * time.Second})
//	summary.Print(os.Stdout)
//
// This package is a thin facade over resonate's internal packages
// (generator, engine, report) — it re-exports their public types via type
// aliases (so values remain fully interchangeable with any code that still
// imports the internal packages directly, e.g. within this module's own
// cmd/resonate) and re-exports their functions as simple wrappers. The
// internal packages remain free to change their own internals; this
// facade's surface is the only thing external callers should depend on.
package resonate
