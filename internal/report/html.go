package report

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"time"
)

//go:embed report.html.tmpl
var htmlTemplateSrc string

var htmlTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"mul100":  func(f float64) float64 { return f * 100 },
	"roundMs": func(d time.Duration) string { return d.Round(time.Millisecond).String() },
	"roundUs": func(d time.Duration) string { return d.Round(time.Microsecond).String() },
}).Parse(htmlTemplateSrc))

// HTMLReport is everything WriteHTML needs beyond the Summary itself: the
// run's identity (for the report's title) and, for a scenario with
// assertions, the pass/fail results so the report can show them the same
// way the CLI's stderr output does.
type HTMLReport struct {
	Title       string // e.g. the target URL or scenario file name
	GeneratedAt time.Time
	Summary     Summary
	Assertions  []Assertion
	Failures    []Failure
}

// WriteHTML renders a single self-contained HTML file (inline CSS, inline
// SVG charts, no external stylesheets/scripts/CDNs) summarizing r: KPI
// cards, a requests/success over time chart, a response time distribution
// histogram, and status code/error breakdowns.
func WriteHTML(w io.Writer, r HTMLReport) error {
	if r.GeneratedAt.IsZero() {
		r.GeneratedAt = time.Now()
	}
	data := htmlData{
		Title:            r.Title,
		GeneratedAt:      r.GeneratedAt.Format("2006-01-02 15:04:05 MST"),
		S:                r.Summary,
		AssertionResults: assertionResults(r.Summary, r.Assertions, r.Failures),
		StatusCodes:      sortedStatusCodes(r.Summary.StatusCodes),
		Errors:           sortedErrors(r.Summary.Errors),
		ThroughputChart:  throughputSVG(r.Summary.TimeSeries),
		HistogramChart:   histogramSVG(r.Summary.LatencyHistogram),
	}
	return htmlTemplate.Execute(w, data)
}

type htmlData struct {
	Title            string
	GeneratedAt      string
	S                Summary
	AssertionResults []assertionResult
	StatusCodes      []statusCodeRow
	Errors           []errorRow
	ThroughputChart  template.HTML
	HistogramChart   template.HTML
}

type assertionResult struct {
	Assertion
	Passed bool
	Detail string
}

type statusCodeRow struct {
	Code  int
	Count int
}

type errorRow struct {
	Message string
	Count   int
}

func assertionResults(s Summary, assertions []Assertion, failures []Failure) []assertionResult {
	if len(assertions) == 0 {
		return nil
	}
	failed := make(map[string]bool, len(failures))
	for _, f := range failures {
		failed[f.Metric] = true
	}
	results := make([]assertionResult, 0, len(assertions))
	for _, a := range assertions {
		actual, err := s.metricValue(a.Metric)
		if err != nil {
			continue
		}
		isFailed := failed[a.Metric]
		detail := Failure{Metric: a.Metric, Actual: actual, Reasons: a.describe()}.String()
		results = append(results, assertionResult{Assertion: a, Passed: !isFailed, Detail: detail})
	}
	return results
}

func sortedStatusCodes(codes map[int]int) []statusCodeRow {
	rows := make([]statusCodeRow, 0, len(codes))
	for c, n := range codes {
		rows = append(rows, statusCodeRow{Code: c, Count: n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Code < rows[j].Code })
	return rows
}

func sortedErrors(errors map[string]int) []errorRow {
	rows := make([]errorRow, 0, len(errors))
	for msg, n := range errors {
		rows = append(rows, errorRow{Message: msg, Count: n})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
	return rows
}

// --- SVG chart rendering (server-side, no JS) ---

const chartW, chartH = 900.0, 220.0

// throughputSVG draws the "Requests over time" chart: stacked bars of requests
// per time bucket (success in green, failure in red-ish) on a grid, with the time
// of day along the x axis and a tooltip for each bucket on hover.
//
// It is still plain SVG plus CSS, with no JavaScript, so the report stays a
// static self-contained file. The hover works like this: every bucket gets a
// transparent hit area covering its whole column (so the tooltip appears when you
// point anywhere in the column, not just at the bar), immediately followed by its
// tooltip group; the stylesheet shows the tooltip with `.hit:hover + .tip`. All the
// hit areas and tooltips come after the bars so a tooltip is never painted under one.
func throughputSVG(buckets []TimeBucket) template.HTML {
	if len(buckets) == 0 {
		return template.HTML(`<p class="empty">No data.</p>`)
	}
	maxReq := 1
	for _, b := range buckets {
		if b.Requests > maxReq {
			maxReq = b.Requests
		}
	}
	width := bucketWidth(buckets)

	// Scale the y axis to a round top (e.g. 100, not 97) so the gridlines fall on
	// round numbers.
	step := niceStep(float64(maxReq), 4)
	top := math.Ceil(float64(maxReq)/step) * step

	const padL, padB, padT = 40.0, 22.0, 4.0
	plotW := chartW - padL - 4
	plotH := chartH - padB - padT
	barW := plotW / float64(len(buckets))

	buf := &htmlBuf{}
	buf.writeHeader()

	// Horizontal gridlines with the request count.
	for v := 0.0; v <= top+step/1000; v += step {
		y := padT + plotH*(1-v/top)
		buf.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, y, chartW-2, y))
		buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%d</text>`, padL-6, y+3.5, int(v+0.5)))
	}
	// Vertical gridlines with the time of day, at bucket boundaries.
	every := tickEvery(len(buckets), int(plotW/80))
	for i := 0; i < len(buckets); i += every {
		x := padL + float64(i)*barW
		buf.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid grid-v"/>`, x, padT, x, padT+plotH))
		// A label centred on a gridline near the right border would be clipped, so
		// those are right-aligned to the border instead.
		anchor, lx := "middle", x
		if x > chartW-32 {
			anchor, lx = "end", chartW-2
		}
		buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis" text-anchor="%s">%s</text>`, lx, chartH-7, anchor, buckets[i].Start.Format("15:04:05")))
	}
	if width != time.Second {
		// Past 120s a run is split into ~120 buckets, so a bar is no longer one second.
		buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis" text-anchor="end">each bar = %s</text>`, chartW-4, padT+11, width.Round(time.Millisecond)))
	}

	for i, b := range buckets {
		if b.Requests == 0 {
			continue
		}
		successFrac := float64(b.Success) / top
		failFrac := float64(b.Requests-b.Success) / top
		x := padL + float64(i)*barW
		failH := plotH * failFrac
		succH := plotH * successFrac
		y := padT + plotH - succH - failH
		buf.WriteString(fmt.Sprintf(`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" class="bar-success"/>`,
			x, y, maxF(barW-0.5, 0.5), succH))
		if failH > 0 {
			buf.WriteString(fmt.Sprintf(`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" class="bar-fail"/>`,
				x, y+succH, maxF(barW-0.5, 0.5), failH))
		}
	}

	// Hover layer: one transparent column per bucket, each followed by its tooltip.
	const tipW, tipH = 196.0, 54.0
	for i, b := range buckets {
		x := padL + float64(i)*barW
		buf.WriteString(fmt.Sprintf(`<rect x="%.2f" y="%.1f" width="%.2f" height="%.1f" class="hit"/>`, x, padT, barW, plotH))

		tx := x + barW + 6
		if tx+tipW > chartW-2 { // near the right edge: open to the left of the column
			tx = x - 6 - tipW
		}
		ty := padT + 4
		buf.WriteString(fmt.Sprintf(`<g class="tip"><rect x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="4" class="tip-bg"/>`, tx, ty, tipW, tipH))
		buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="tip-t">%s  (+%s)</text>`, tx+9, ty+15,
			b.Start.Format("15:04:05"), b.Start.Sub(buckets[0].Start).Round(time.Second)))
		if b.Requests == 0 {
			buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="tip-v">no requests</text>`, tx+9, ty+34))
		} else {
			buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="tip-v">%d requests (%d ok, %d failed)</text>`,
				tx+9, ty+34, b.Requests, b.Success, b.Requests-b.Success))
			buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="tip-m">%.1f/s · mean %s</text>`,
				tx+9, ty+48, float64(b.Requests)/width.Seconds(), b.Mean.Round(time.Microsecond)))
		}
		buf.WriteString(`</g>`)
	}
	buf.writeFooter()
	return template.HTML(buf.String())
}

// bucketWidth is the span one TimeBucket covers: the gap between neighbouring
// buckets, or a second for a one-bucket series.
func bucketWidth(buckets []TimeBucket) time.Duration {
	if len(buckets) >= 2 {
		if d := buckets[1].Start.Sub(buckets[0].Start); d > 0 {
			return d
		}
	}
	return time.Second
}

// niceStep picks a round axis step (1, 2 or 5 times a power of ten, and at least
// 1, since these are counts) so that about `ticks` steps cover max.
func niceStep(max float64, ticks int) float64 {
	if max <= 0 || ticks <= 0 {
		return 1
	}
	raw := max / float64(ticks)
	exp := math.Pow(10, math.Floor(math.Log10(raw)))
	var nice float64
	switch f := raw / exp; {
	case f <= 1:
		nice = 1
	case f <= 2:
		nice = 2
	case f <= 5:
		nice = 5
	default:
		nice = 10
	}
	return math.Max(nice*exp, 1)
}

// tickEvery returns how many buckets apart to place time labels so that a series
// of n buckets gets at most maxLabels of them, using round spacings.
func tickEvery(n, maxLabels int) int {
	if maxLabels < 2 {
		maxLabels = 2
	}
	for _, e := range []int{1, 2, 5, 10, 15, 20, 30, 60, 120, 300, 600, 1200, 3600} {
		if (n+e-1)/e <= maxLabels {
			return e
		}
	}
	return 3600
}

// histogramSVG draws a response-time distribution bar chart from bins.
func histogramSVG(bins []HistogramBin) template.HTML {
	if len(bins) == 0 {
		return template.HTML(`<p class="empty">No data.</p>`)
	}
	maxCount := 1
	for _, b := range bins {
		if b.Count > maxCount {
			maxCount = b.Count
		}
	}

	const padL, padB = 40.0, 20.0
	plotW := chartW - padL - 4
	plotH := chartH - padB - 4
	barW := plotW / float64(len(bins))

	buf := &htmlBuf{}
	buf.writeHeader()
	for i := 0; i <= 2; i++ {
		frac := float64(i) / 2
		y := 4 + plotH*(1-frac)
		val := int(float64(maxCount) * frac)
		buf.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, y, chartW-2, y))
		buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis">%d</text>`, padL-6, y+4, val))
	}
	for i, b := range bins {
		frac := float64(b.Count) / float64(maxCount)
		h := plotH * frac
		x := padL + float64(i)*barW
		y := 4 + plotH - h
		buf.WriteString(fmt.Sprintf(`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" class="bar-hist"><title>&le; %s: %d</title></rect>`,
			x, y, maxF(barW-0.5, 0.5), h, b.Le.Round(time.Millisecond), b.Count))
	}
	buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis">%s</text>`, padL, chartH-4, bins[0].Le.Round(time.Millisecond)))
	buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`, chartW-2, chartH-4, bins[len(bins)-1].Le.Round(time.Millisecond)))
	buf.writeFooter()
	return template.HTML(buf.String())
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// htmlBuf is a tiny string builder wrapper so the chart functions above read
// as a flat sequence of writes instead of threading a strings.Builder
// through every helper call.
type htmlBuf struct {
	s string
}

func (b *htmlBuf) WriteString(s string) { b.s += s }
func (b *htmlBuf) String() string       { return b.s }

func (b *htmlBuf) writeHeader() {
	b.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %g %g" xmlns="http://www.w3.org/2000/svg" class="chart">`, chartW, chartH))
}

func (b *htmlBuf) writeFooter() {
	b.WriteString(`</svg>`)
}
