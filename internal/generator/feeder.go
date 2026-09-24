package generator

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Feeder cycles or randomly samples rows of string-keyed data from a CSV or
// JSON file, exposed to templates as {{.Feeder.<column>}}: drive requests
// from a real-looking dataset instead of purely random values.
//
// "sequential"/"random" mode load the whole file into memory once, at
// construction — simple and fast, but unsuitable for a file too large to
// comfortably fit in RAM (e.g. an exported production dataset). "stream"
// mode instead reads the file incrementally via a background goroutine,
// holding only a small read-ahead buffer in memory regardless of file
// size; see streamFeeder.
type Feeder struct {
	rows    []map[string]string
	random  bool
	counter uint64 // atomic; only advanced in sequential mode

	stream *streamFeeder // non-nil in "stream" mode; rows/random/counter unused then
}

// NewFeeder opens path (.csv or .json). mode is "sequential" (default: rows
// handed out round-robin, wrapping around), "random" (a row chosen
// uniformly at random each call), or "stream" (like sequential — round-
// robin, wraps around — but read incrementally instead of loaded into
// memory all at once; see streamFeeder). Construction fails fast on a
// missing file, malformed data, or empty dataset, before any request is
// sent rather than mid-run.
func NewFeeder(path, mode string) (*Feeder, error) {
	switch mode {
	case "", "sequential", "random", "stream":
	default:
		return nil, fmt.Errorf("feeder: invalid mode %q (want \"sequential\", \"random\", or \"stream\")", mode)
	}
	if mode == "" {
		mode = "sequential"
	}

	format := strings.ToLower(filepath.Ext(path))
	if format != ".csv" && format != ".json" {
		return nil, fmt.Errorf("feeder: unsupported file extension for %q (want .csv or .json)", path)
	}

	if mode == "stream" {
		sf, err := newStreamFeeder(path, format)
		if err != nil {
			return nil, err
		}
		return &Feeder{stream: sf}, nil
	}

	var rows []map[string]string
	var err error
	switch format {
	case ".csv":
		rows, err = loadCSVFeeder(path)
	case ".json":
		rows, err = loadJSONFeeder(path)
	}
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("feeder: %q has no data rows", path)
	}

	return &Feeder{rows: rows, random: mode == "random"}, nil
}

func loadCSVFeeder(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("feeder: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("feeder: reading %q header: %w", path, err)
	}

	var rows []map[string]string
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("feeder: reading %q: %w", path, err)
		}
		row := make(map[string]string, len(header))
		for i, col := range header {
			row[col] = record[i]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func loadJSONFeeder(path string) ([]map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("feeder: %w", err)
	}

	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("feeder: %q must be a JSON array of objects: %w", path, err)
	}

	rows := make([]map[string]string, len(raw))
	for i, obj := range raw {
		row := make(map[string]string, len(obj))
		for k, v := range obj {
			row[k] = stringifyJSONValue(v)
		}
		rows[i] = row
	}
	return rows, nil
}

// stringifyJSONValue renders a decoded JSON value as a template-friendly
// string: strings pass through as-is, other scalars use their natural
// textual form, and objects/arrays re-marshal to compact JSON so a template
// can still reference them (e.g. {{.Feeder.metadata}}) instead of erroring.
func stringifyJSONValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

// Next returns the next row: round-robin in sequential/stream mode,
// uniformly random in random mode. Safe for concurrent use by multiple
// goroutines, and safe to call on a nil *Feeder (returns nil, like an
// unset field).
func (f *Feeder) Next() map[string]string {
	if f == nil {
		return nil
	}
	if f.stream != nil {
		return f.stream.next()
	}
	if f.random {
		return f.rows[rand.IntN(len(f.rows))]
	}
	idx := atomic.AddUint64(&f.counter, 1) - 1
	return f.rows[idx%uint64(len(f.rows))]
}

// Close stops "stream" mode's background reader goroutine and releases its
// open file handle. A no-op for sequential/random Feeders (nothing to
// stop) and safe to call on a nil *Feeder. Callers should only call this
// once every goroutine that might call Next() has finished — see
// streamFeeder's doc comment.
func (f *Feeder) Close() {
	if f == nil || f.stream == nil {
		return
	}
	f.stream.close()
}

// streamFeederBufferSize bounds how many rows streamFeeder holds in memory
// at once (read-ahead buffer), independent of the underlying file's size.
const streamFeederBufferSize = 1000

// streamReopenBackoff is how long streamFeeder waits between retries when
// (re)opening its file fails — e.g. the file is briefly unavailable, or
// (pathologically) it was truncated to empty after construction already
// validated it had data.
const streamReopenBackoff = 100 * time.Millisecond

// streamFeeder reads rows from a CSV/JSON file without loading it all into
// memory: a background goroutine reads one row at a time via rowSource and
// feeds a small buffered channel; Next() just receives from it. On reaching
// the end of the file, the goroutine reopens it and starts again — like
// sequential mode, it wraps around indefinitely. Memory use stays bounded
// to streamFeederBufferSize rows regardless of file size.
//
// The reader goroutine runs until close() is called; it's only safe to
// call close() once nothing can still be calling next() (Feeder.Close()'s
// doc comment carries the same caveat) — a next() call racing a close()
// could otherwise block forever waiting on a channel nothing will ever
// send to again. In this codebase that's satisfied by construction: every
// Generator.Close() (which calls Feeder.Close()) only runs after
// engine.Run has returned, and Run doesn't return until every worker
// goroutine — the only callers of Next() — has already stopped.
type streamFeeder struct {
	rows chan map[string]string
	stop chan struct{}
	once sync.Once
}

// newStreamFeeder validates path/format the same way NewFeeder's other
// modes do — opens it, confirms the header/array-start parses, and reads
// one row to confirm the dataset isn't empty — all before starting the
// background reader, so a bad file fails fast at construction. The
// validating read is done with its own rowSource, immediately closed; the
// background goroutine opens its own from scratch.
func newStreamFeeder(path, format string) (*streamFeeder, error) {
	src, err := openRowSource(path, format)
	if err != nil {
		return nil, err
	}
	_, err = src.next()
	src.close()
	if err != nil {
		return nil, fmt.Errorf("feeder: %q has no data rows", path)
	}

	sf := &streamFeeder{
		rows: make(chan map[string]string, streamFeederBufferSize),
		stop: make(chan struct{}),
	}
	go sf.run(path, format)
	return sf, nil
}

func (sf *streamFeeder) run(path, format string) {
	src := sf.openWithRetry(path, format)
	if src == nil {
		return // stop was closed before we ever got a working reader
	}
	for {
		row, err := src.next()
		if err != nil {
			src.close()
			src = sf.openWithRetry(path, format)
			if src == nil {
				return
			}
			continue
		}
		select {
		case sf.rows <- row:
		case <-sf.stop:
			src.close()
			return
		}
	}
}

// openWithRetry opens path, retrying with a backoff until it succeeds or
// stop is closed (in which case it returns nil). Used both for the
// initial open and to reopen on EOF ("wrap around").
func (sf *streamFeeder) openWithRetry(path, format string) *rowSource {
	for {
		src, err := openRowSource(path, format)
		if err == nil {
			return src
		}
		select {
		case <-time.After(streamReopenBackoff):
		case <-sf.stop:
			return nil
		}
	}
}

func (sf *streamFeeder) next() map[string]string {
	return <-sf.rows
}

func (sf *streamFeeder) close() {
	sf.once.Do(func() { close(sf.stop) })
}

// rowSource reads one row at a time from an open CSV or JSON file, for
// streamFeeder. Each rowSource is single-use and single-reader (no
// internal locking) — streamFeeder only ever has one goroutine driving one
// rowSource at a time.
type rowSource struct {
	file   *os.File
	format string // ".csv" or ".json"

	csvReader *csv.Reader
	csvHeader []string

	jsonDec *json.Decoder
}

func openRowSource(path, format string) (*rowSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("feeder: %w", err)
	}
	rs := &rowSource{file: f, format: format}

	switch format {
	case ".csv":
		rs.csvReader = csv.NewReader(f)
		header, err := rs.csvReader.Read()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("feeder: reading %q header: %w", path, err)
		}
		rs.csvHeader = header
	case ".json":
		rs.jsonDec = json.NewDecoder(f)
		tok, err := rs.jsonDec.Token()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("feeder: reading %q: %w", path, err)
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			f.Close()
			return nil, fmt.Errorf("feeder: %q must be a JSON array of objects", path)
		}
	}
	return rs, nil
}

// next returns the next row, or an error (including io.EOF at the end of
// the file/array) if there isn't one.
func (rs *rowSource) next() (map[string]string, error) {
	switch rs.format {
	case ".csv":
		record, err := rs.csvReader.Read()
		if err != nil {
			return nil, err
		}
		row := make(map[string]string, len(rs.csvHeader))
		for i, col := range rs.csvHeader {
			row[col] = record[i]
		}
		return row, nil
	default: // ".json"
		if !rs.jsonDec.More() {
			return nil, io.EOF
		}
		var obj map[string]any
		if err := rs.jsonDec.Decode(&obj); err != nil {
			return nil, err
		}
		row := make(map[string]string, len(obj))
		for k, v := range obj {
			row[k] = stringifyJSONValue(v)
		}
		return row, nil
	}
}

func (rs *rowSource) close() {
	rs.file.Close()
}
