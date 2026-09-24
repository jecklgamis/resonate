package report

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
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

// throughputSVG draws a stacked bar chart of requests/sec over time
// (success in green, failure in red-ish), one bar per TimeBucket.
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

	const padL, padB = 40.0, 20.0
	plotW := chartW - padL - 4
	plotH := chartH - padB - 4
	barW := plotW / float64(len(buckets))

	buf := &htmlBuf{}
	buf.writeHeader()
	// y-axis gridlines/labels (0, max/2, max)
	for i := 0; i <= 2; i++ {
		frac := float64(i) / 2
		y := 4 + plotH*(1-frac)
		val := int(float64(maxReq) * frac)
		buf.WriteString(fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, y, chartW-2, y))
		buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis">%d</text>`, padL-6, y+4, val))
	}
	for i, b := range buckets {
		successFrac := 0.0
		failFrac := 0.0
		if b.Requests > 0 {
			successFrac = float64(b.Success) / float64(maxReq)
			failFrac = float64(b.Requests-b.Success) / float64(maxReq)
		}
		x := padL + float64(i)*barW
		failH := plotH * failFrac
		succH := plotH * successFrac
		y := 4 + plotH - succH - failH
		if b.Requests > 0 {
			buf.WriteString(fmt.Sprintf(`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" class="bar-success"><title>%s: %d req (%d ok)</title></rect>`,
				x, y, maxF(barW-0.5, 0.5), succH, b.Start.Format("15:04:05"), b.Requests, b.Success))
			if failH > 0 {
				buf.WriteString(fmt.Sprintf(`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" class="bar-fail"><title>%s: %d failed</title></rect>`,
					x, y+succH, maxF(barW-0.5, 0.5), failH, b.Start.Format("15:04:05"), b.Requests-b.Success))
			}
		}
	}
	buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis">%s</text>`, padL, chartH-4, buckets[0].Start.Format("15:04:05")))
	buf.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`, chartW-2, chartH-4, buckets[len(buckets)-1].Start.Format("15:04:05")))
	buf.writeFooter()
	return template.HTML(buf.String())
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
