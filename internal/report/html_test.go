package report

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
)

func TestBucketSeriesCountsRequestsAndSuccess(t *testing.T) {
	agg := NewAggregator()
	start := time.Now()
	agg.Add(generator.Result{Timestamp: start, Latency: time.Millisecond, StatusCode: 200})
	agg.Add(generator.Result{Timestamp: start.Add(500 * time.Millisecond), Latency: time.Millisecond, StatusCode: 500})
	agg.Add(generator.Result{Timestamp: start.Add(2 * time.Second), Latency: time.Millisecond, StatusCode: 200})

	s := agg.Summary(3 * time.Second)
	if len(s.TimeSeries) == 0 {
		t.Fatal("expected a non-empty TimeSeries")
	}
	total, success := 0, 0
	for _, b := range s.TimeSeries {
		total += b.Requests
		success += b.Success
	}
	if total != 3 {
		t.Errorf("total bucketed requests = %d, want 3", total)
	}
	if success != 2 {
		t.Errorf("total bucketed success = %d, want 2 (500 doesn't count)", success)
	}
}

func TestBucketSeriesEmptyWhenNoPoints(t *testing.T) {
	agg := NewAggregator()
	s := agg.Summary(time.Second)
	if s.TimeSeries != nil {
		t.Errorf("TimeSeries = %+v, want nil for an empty run", s.TimeSeries)
	}
}

func TestLatencyHistogramCoversAllSamples(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	for i := 1; i <= 50; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(i) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)

	if len(s.LatencyHistogram) == 0 {
		t.Fatal("expected a non-empty LatencyHistogram")
	}
	total := 0
	for _, b := range s.LatencyHistogram {
		total += b.Count
	}
	if total != 50 {
		t.Errorf("histogram total = %d, want 50", total)
	}
	if s.LatencyHistogram[len(s.LatencyHistogram)-1].Le != 50*time.Millisecond {
		t.Errorf("last bin Le = %v, want 50ms (the max)", s.LatencyHistogram[len(s.LatencyHistogram)-1].Le)
	}
}

func TestLatencyHistogramSingleValue(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	agg.Add(generator.Result{Timestamp: now, Latency: 5 * time.Millisecond, StatusCode: 200})
	agg.Add(generator.Result{Timestamp: now, Latency: 5 * time.Millisecond, StatusCode: 200})
	s := agg.Summary(time.Second)

	if len(s.LatencyHistogram) != 1 {
		t.Fatalf("got %d bins, want 1 when every latency is identical", len(s.LatencyHistogram))
	}
	if s.LatencyHistogram[0].Count != 2 {
		t.Errorf("bin count = %d, want 2", s.LatencyHistogram[0].Count)
	}
}

func TestWriteHTMLProducesSelfContainedPage(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	agg.Add(generator.Result{Timestamp: now, Latency: 10 * time.Millisecond, StatusCode: 200})
	agg.Add(generator.Result{Timestamp: now.Add(time.Second), Latency: 20 * time.Millisecond, StatusCode: 500})
	s := agg.Summary(2 * time.Second)

	min := 0.0
	max := 0.6
	var buf bytes.Buffer
	err := WriteHTML(&buf, HTMLReport{
		Title:      "https://example.com",
		Summary:    s,
		Assertions: []Assertion{{Metric: "success_rate", Min: &min, Max: &max}},
		Failures:   nil,
	})
	if err != nil {
		t.Fatalf("WriteHTML error: %v", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Error("output does not start with a doctype")
	}
	for _, want := range []string{"<style>", "<svg", "https://example.com", "success_rate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	for _, forbidden := range []string{"cdn.", "<script", "<link rel=\"stylesheet\""} { // no scripts at all: the hover is CSS-only
		if strings.Contains(out, forbidden) {
			t.Errorf("output unexpectedly references an external resource: %q", forbidden)
		}
	}
}

func TestWriteHTMLHandlesEmptySummary(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, HTMLReport{Title: "empty", Summary: Summary{}}); err != nil {
		t.Fatalf("WriteHTML error on empty summary: %v", err)
	}
	if !strings.Contains(buf.String(), "No data.") {
		t.Error("expected charts to render a 'No data.' placeholder for an empty summary")
	}
}

// --- "Requests over time" chart: grid, time axis and hover ---

func testBuckets(n int, width time.Duration, f func(i int) (req, ok int)) []TimeBucket {
	start := time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local)
	out := make([]TimeBucket, n)
	for i := range out {
		r, ok := f(i)
		out[i] = TimeBucket{Start: start.Add(time.Duration(i) * width), Requests: r, Success: ok}
		if r > 0 {
			out[i].Mean = 450 * time.Microsecond
		}
	}
	return out
}

func TestThroughputChartHasAGridWithRoundLabelsAndATimeAxis(t *testing.T) {
	// 31 one-second buckets peaking at 97: the y axis tops out at a round 100, in
	// steps from the 1-2-5 sequence (50 here).
	buckets := testBuckets(31, time.Second, func(i int) (int, int) {
		if i == 7 {
			return 97, 97
		}
		return 40, 40
	})
	out := string(throughputSVG(buckets))

	for _, label := range []string{">0<", ">50<", ">100<"} {
		if !strings.Contains(out, label) {
			t.Errorf("y axis is missing the round label %q", label)
		}
	}
	if got := strings.Count(out, `class="grid"/>`); got != 3 {
		t.Errorf("want 3 horizontal gridlines (0, 50, 100), got %d", got)
	}

	// Time of day along the x axis, one dashed vertical gridline per label.
	every := tickEvery(31, 10)
	wantTicks := (31 + every - 1) / every
	if got := strings.Count(out, `class="grid grid-v"/>`); got != wantTicks {
		t.Errorf("want %d vertical gridlines (a label every %d buckets), got %d", wantTicks, every, got)
	}
	for i := 0; i < 31; i += every {
		if label := ">" + buckets[i].Start.Format("15:04:05") + "<"; !strings.Contains(out, label) {
			t.Errorf("x axis is missing the time label %q for bucket %d", label, i)
		}
	}
}

func TestThroughputChartLastTimeLabelIsNotClippedAtTheRightEdge(t *testing.T) {
	// 121 buckets (a long run) puts a tick right at the border.
	buckets := testBuckets(121, 5*time.Second, func(i int) (int, int) { return 300, 300 })
	out := string(throughputSVG(buckets))
	last := buckets[120].Start.Format("15:04:05")
	if !strings.Contains(out, `text-anchor="end">`+last+`<`) {
		t.Errorf("the final label %s sits on the right border and must be right-aligned so it isn't clipped", last)
	}
}

func TestThroughputChartHoverLayerPairsEachColumnWithItsTooltip(t *testing.T) {
	buckets := testBuckets(10, time.Second, func(i int) (int, int) {
		switch i {
		case 3:
			return 0, 0
		case 4:
			return 60, 45
		}
		return 50, 50
	})
	out := string(throughputSVG(buckets))

	// The stylesheet shows a tooltip with `.hit:hover + .tip`, which only works if
	// each hit area is immediately followed by its tooltip group.
	if got := strings.Count(out, `class="hit"/><g class="tip">`); got != len(buckets) {
		t.Fatalf("want every one of the %d columns immediately followed by its tooltip, got %d pairs", len(buckets), got)
	}
	// Tooltips must come after the last bar, or a bar would be painted over them.
	if strings.LastIndex(out, `class="bar-success"`) > strings.Index(out, `class="hit"`) {
		t.Error("a bar is drawn after the hover layer, so it could cover a tooltip")
	}

	// A bucket's tooltip carries the time of day, the offset, the counts, the rate and the mean latency.
	for _, want := range []string{
		buckets[4].Start.Format("15:04:05") + "  (+4s)",
		"60 requests (45 ok, 15 failed)",
		"60.0/s · mean 450µs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the tooltip is missing %q", want)
		}
	}
	if !strings.Contains(out, "no requests") {
		t.Error("an empty bucket's tooltip should say so")
	}
}

func TestThroughputChartTooltipOpensTowardsTheCentre(t *testing.T) {
	buckets := testBuckets(30, time.Second, func(i int) (int, int) { return 50, 50 })
	out := string(throughputSVG(buckets))

	hitX := regexp.MustCompile(`<rect x="([0-9.]+)" y="[0-9.]+" width="[0-9.]+" height="[0-9.]+" class="hit"/>`).FindAllStringSubmatch(out, -1)
	tipX := regexp.MustCompile(`<g class="tip"><rect x="(-?[0-9.]+)"`).FindAllStringSubmatch(out, -1)
	if len(hitX) != 30 || len(tipX) != 30 {
		t.Fatalf("want 30 columns and 30 tooltips, got %d and %d", len(hitX), len(tipX))
	}
	x := func(m [][]string, i int) float64 {
		v, err := strconv.ParseFloat(m[i][1], 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if x(tipX, 0) <= x(hitX, 0) {
		t.Error("the first column's tooltip should open to its right")
	}
	if last := len(hitX) - 1; x(tipX, last) >= x(hitX, last) {
		t.Error("the last column's tooltip should open to its left, or it would run off the chart")
	}
	for i := range tipX {
		if x(tipX, i) < 0 || x(tipX, i)+196 > chartW {
			t.Errorf("tooltip %d is outside the chart (x=%v)", i, x(tipX, i))
		}
	}
}

func TestThroughputChartStatesBucketWidthWhenItIsNotOneSecond(t *testing.T) {
	// Past 120s a run is split into ~120 buckets, so a bar covers 5s here and the
	// tooltip's rate is per second, not per bucket.
	long := string(throughputSVG(testBuckets(10, 5*time.Second, func(i int) (int, int) { return 250, 250 })))
	if !strings.Contains(long, "each bar = 5s") {
		t.Error("a chart whose bars are 5s wide should say so")
	}
	if !strings.Contains(long, "250 requests (250 ok, 0 failed)") || !strings.Contains(long, "50.0/s") {
		t.Error("250 requests in a 5s bucket is 50.0/s; the tooltip should show both")
	}

	short := string(throughputSVG(testBuckets(10, time.Second, func(i int) (int, int) { return 50, 50 })))
	if strings.Contains(short, "each bar =") {
		t.Error("one-second bars need no caption")
	}
}

func TestThroughputChartKeepsFailuresAtTheBaseOfTheStack(t *testing.T) {
	out := string(throughputSVG(testBuckets(2, time.Second, func(i int) (int, int) { return 100, 60 })))
	re := regexp.MustCompile(`<rect x="[0-9.]+" y="([0-9.]+)" width="[0-9.]+" height="([0-9.]+)" class="bar-(success|fail)"/>`)
	m := re.FindAllStringSubmatch(out, -1)
	if len(m) < 2 || m[0][3] != "success" || m[1][3] != "fail" {
		t.Fatalf("expected a success bar then a failure bar per column, got %v", m)
	}
	succY, _ := strconv.ParseFloat(m[0][1], 64)
	succH, _ := strconv.ParseFloat(m[0][2], 64)
	failY, _ := strconv.ParseFloat(m[1][1], 64)
	if failY < succY+succH-0.01 {
		t.Errorf("failures (y=%v) should sit below the successes (y=%v, h=%v)", failY, succY, succH)
	}
}

func TestChartHelpers(t *testing.T) {
	// Steps come from the 1-2-5 sequence (times a power of ten), and are at least 1.
	for max, want := range map[float64]float64{0: 1, 1: 1, 3: 1, 4: 1, 9: 5, 97: 50, 100: 50, 480: 200, 600: 200, 9999: 5000} {
		if got := niceStep(max, 4); got != want {
			t.Errorf("niceStep(%v, 4) = %v, want %v", max, got, want)
		}
	}
	for n, want := range map[int]int{1: 1, 10: 1, 31: 5, 61: 10, 121: 15, 7200: 1200} {
		if got := tickEvery(n, 10); got != want {
			t.Errorf("tickEvery(%d, 10) = %d, want %d", n, got, want)
		}
	}
	if got := bucketWidth(testBuckets(3, 5*time.Second, func(int) (int, int) { return 1, 1 })); got != 5*time.Second {
		t.Errorf("bucketWidth = %v, want 5s", got)
	}
	if got := bucketWidth(testBuckets(1, 5*time.Second, func(int) (int, int) { return 1, 1 })); got != time.Second {
		t.Errorf("a single bucket should default to 1s, got %v", got)
	}
}

func TestWriteHTMLLinksTheRepoAndShowsP75(t *testing.T) {
	var buf bytes.Buffer
	s := Summary{Requests: 1, Latencies: LatencyStats{P75: 1234 * time.Microsecond}, TimeSeries: testBuckets(3, time.Second, func(int) (int, int) { return 5, 5 })}
	if err := WriteHTML(&buf, HTMLReport{Title: "t", Summary: s}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`href="https://github.com/jecklgamis/resonate"`,
		`rel="noopener noreferrer"`,
		"<th>p75</th>",
		fmt.Sprint("<td>", "1.234ms", "</td>"),
		".hit:hover + .tip",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	// p75 sits between p50 and p90 in the table.
	if i50, i75, i90 := strings.Index(out, "<th>p50</th>"), strings.Index(out, "<th>p75</th>"), strings.Index(out, "<th>p90</th>"); !(i50 < i75 && i75 < i90) {
		t.Error("the p75 column should sit between p50 and p90")
	}
}
