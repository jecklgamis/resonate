package resonate

import "testing"

// MaxIdleConns has no reliable black-box signal: how many connections the
// transport keeps idle depends on goroutine timing, so a request-level test
// of it would be flaky. The setter is one assignment; check it lands on the
// options the generator is built from.
func TestMaxIdleConnsReachesTheHTTPOptions(t *testing.T) {
	s := NewScenario().Get("http://localhost:1/").MaxIdleConns(250)
	if s.httpOpts.MaxIdleConns != 250 {
		t.Errorf("httpOpts.MaxIdleConns = %d, want 250", s.httpOpts.MaxIdleConns)
	}
}
