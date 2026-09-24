package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTempDataFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}

func TestNewFeederCSVLoadsRows(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email,password\nalice@example.com,pw1\nbob@example.com,pw2\n")
	f, err := NewFeeder(path, "")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	if len(f.rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(f.rows))
	}
	if f.rows[0]["email"] != "alice@example.com" || f.rows[0]["password"] != "pw1" {
		t.Errorf("row 0 = %+v, unexpected", f.rows[0])
	}
	if f.rows[1]["email"] != "bob@example.com" {
		t.Errorf("row 1 = %+v, unexpected", f.rows[1])
	}
}

func TestNewFeederCSVMismatchedColumnsErrors(t *testing.T) {
	path := writeTempDataFile(t, "bad.csv", "email,password\nalice@example.com\n")
	if _, err := NewFeeder(path, ""); err == nil {
		t.Fatal("expected an error for a row with fewer columns than the header")
	}
}

func TestNewFeederJSONLoadsRows(t *testing.T) {
	path := writeTempDataFile(t, "users.json", `[
		{"email": "alice@example.com", "age": 30, "active": true},
		{"email": "bob@example.com", "age": 25, "active": false}
	]`)
	f, err := NewFeeder(path, "")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	if len(f.rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(f.rows))
	}
	if f.rows[0]["email"] != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", f.rows[0]["email"])
	}
	if f.rows[0]["age"] != "30" {
		t.Errorf("age = %q, want \"30\" (number stringified)", f.rows[0]["age"])
	}
	if f.rows[0]["active"] != "true" {
		t.Errorf("active = %q, want \"true\" (bool stringified)", f.rows[0]["active"])
	}
}

func TestNewFeederJSONNestedValueReencodesAsJSON(t *testing.T) {
	path := writeTempDataFile(t, "nested.json", `[{"meta": {"tier": "gold"}}]`)
	f, err := NewFeeder(path, "")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	if f.rows[0]["meta"] != `{"tier":"gold"}` {
		t.Errorf("meta = %q, want the nested object re-encoded as compact JSON", f.rows[0]["meta"])
	}
}

func TestNewFeederJSONNotAnArrayErrors(t *testing.T) {
	path := writeTempDataFile(t, "notarray.json", `{"email": "alice@example.com"}`)
	if _, err := NewFeeder(path, ""); err == nil {
		t.Fatal("expected an error when the JSON top level isn't an array")
	}
}

func TestNewFeederUnsupportedExtensionErrors(t *testing.T) {
	path := writeTempDataFile(t, "users.txt", "email\nalice@example.com\n")
	if _, err := NewFeeder(path, ""); err == nil {
		t.Fatal("expected an error for an unsupported file extension")
	}
}

func TestNewFeederMissingFileErrors(t *testing.T) {
	if _, err := NewFeeder("/nonexistent/path/users.csv", ""); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestNewFeederEmptyDataErrors(t *testing.T) {
	path := writeTempDataFile(t, "empty.csv", "email,password\n")
	if _, err := NewFeeder(path, ""); err == nil {
		t.Fatal("expected an error for a file with a header but no data rows")
	}
}

func TestNewFeederInvalidModeErrors(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email\nalice@example.com\n")
	if _, err := NewFeeder(path, "bogus"); err == nil {
		t.Fatal("expected an error for an invalid mode")
	}
}

func TestFeederNextSequentialWrapsAround(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email\na@example.com\nb@example.com\n")
	f, err := NewFeeder(path, "sequential")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	got := []string{
		f.Next()["email"],
		f.Next()["email"],
		f.Next()["email"], // wraps back to row 0
	}
	want := []string{"a@example.com", "b@example.com", "a@example.com"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Next() #%d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFeederNextRandomStaysWithinRows(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email\na@example.com\nb@example.com\nc@example.com\n")
	f, err := NewFeeder(path, "random")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	valid := map[string]bool{"a@example.com": true, "b@example.com": true, "c@example.com": true}
	for i := 0; i < 50; i++ {
		email := f.Next()["email"]
		if !valid[email] {
			t.Fatalf("Next() produced unexpected value %q", email)
		}
	}
}

func TestFeederNextOnNilFeederReturnsNil(t *testing.T) {
	var f *Feeder
	if got := f.Next(); got != nil {
		t.Errorf("Next() on a nil *Feeder = %v, want nil", got)
	}
}

func TestNewFeederStreamCSVLoadsRows(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email,password\nalice@example.com,pw1\nbob@example.com,pw2\n")
	f, err := NewFeeder(path, "stream")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	t.Cleanup(f.Close)

	row0 := f.Next()
	if row0["email"] != "alice@example.com" || row0["password"] != "pw1" {
		t.Errorf("row 0 = %+v, unexpected", row0)
	}
	row1 := f.Next()
	if row1["email"] != "bob@example.com" {
		t.Errorf("row 1 = %+v, unexpected", row1)
	}
}

func TestNewFeederStreamJSONLoadsRows(t *testing.T) {
	path := writeTempDataFile(t, "users.json", `[
		{"email": "alice@example.com", "age": 30, "active": true},
		{"email": "bob@example.com", "age": 25, "active": false}
	]`)
	f, err := NewFeeder(path, "stream")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	t.Cleanup(f.Close)

	row0 := f.Next()
	if row0["email"] != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", row0["email"])
	}
	if row0["age"] != "30" {
		t.Errorf("age = %q, want \"30\" (number stringified)", row0["age"])
	}
	if row0["active"] != "true" {
		t.Errorf("active = %q, want \"true\" (bool stringified)", row0["active"])
	}
}

func TestFeederNextStreamWrapsAround(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email\na@example.com\nb@example.com\n")
	f, err := NewFeeder(path, "stream")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	t.Cleanup(f.Close)

	got := []string{
		f.Next()["email"],
		f.Next()["email"],
		f.Next()["email"], // wraps back to row 0
		f.Next()["email"],
		f.Next()["email"], // wraps a second time
	}
	want := []string{"a@example.com", "b@example.com", "a@example.com", "b@example.com", "a@example.com"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Next() #%d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewFeederStreamEmptyDataErrors(t *testing.T) {
	path := writeTempDataFile(t, "empty.csv", "email,password\n")
	if _, err := NewFeeder(path, "stream"); err == nil {
		t.Fatal("expected an error for a file with a header but no data rows")
	}
}

func TestNewFeederStreamMissingFileErrors(t *testing.T) {
	if _, err := NewFeeder("/nonexistent/path/users.csv", "stream"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestNewFeederStreamJSONNotAnArrayErrors(t *testing.T) {
	path := writeTempDataFile(t, "notarray.json", `{"email": "alice@example.com"}`)
	if _, err := NewFeeder(path, "stream"); err == nil {
		t.Fatal("expected an error when the JSON top level isn't an array")
	}
}

func TestNewFeederStreamCSVMismatchedColumnsErrors(t *testing.T) {
	path := writeTempDataFile(t, "bad.csv", "email,password\nalice@example.com\n")
	if _, err := NewFeeder(path, "stream"); err == nil {
		t.Fatal("expected an error for a row with fewer columns than the header")
	}
}

func TestFeederStreamCloseIsIdempotentAndSafeOnNil(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email\na@example.com\n")
	f, err := NewFeeder(path, "stream")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	f.Close()
	f.Close() // must not panic/block on a second call

	var nilFeeder *Feeder
	nilFeeder.Close() // must not panic
}

func TestFeederStreamHandlesManyConcurrentReaders(t *testing.T) {
	var b strings.Builder
	b.WriteString("id\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	path := writeTempDataFile(t, "many.csv", b.String())

	f, err := NewFeeder(path, "stream")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	t.Cleanup(f.Close)

	// Enough pulls to force at least one wraparound (50 rows), driven by
	// many goroutines at once, to exercise the channel-based handoff for
	// data races (run with -race) and confirm every pull returns a row.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				row := f.Next()
				if row == nil || row["id"] == "" {
					t.Errorf("Next() returned an empty/nil row: %+v", row)
				}
			}
		}()
	}
	wg.Wait()
}

func TestNewFeederStreamInvalidModeStillRejected(t *testing.T) {
	path := writeTempDataFile(t, "users.csv", "email\na@example.com\n")
	if _, err := NewFeeder(path, "streaming"); err == nil {
		t.Fatal("expected an error for a mode that's close to, but not exactly, \"stream\"")
	}
}

// openWithRetry is exercised directly (rather than through the full
// streamFeeder.run goroutine) so the retry-until-success and
// stop-during-backoff branches can be triggered deterministically instead
// of racing the background reader's own eager reopen-on-EOF.

func TestStreamFeederOpenWithRetryRetriesUntilFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.csv")
	sf := &streamFeeder{stop: make(chan struct{})}

	done := make(chan *rowSource, 1)
	go func() { done <- sf.openWithRetry(path, ".csv") }()

	// Let at least one attempt fail and enter the backoff wait before the
	// file appears.
	time.Sleep(streamReopenBackoff / 2)
	if err := os.WriteFile(path, []byte("email\nalice@example.com\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	select {
	case src := <-done:
		if src == nil {
			t.Fatal("openWithRetry returned nil, want a rowSource once the file appears")
		}
		src.close()
	case <-time.After(2 * time.Second):
		t.Fatal("openWithRetry did not return after the file was created")
	}
}

func TestStreamFeederOpenWithRetryReturnsNilWhenStopped(t *testing.T) {
	sf := &streamFeeder{stop: make(chan struct{})}

	done := make(chan *rowSource, 1)
	go func() { done <- sf.openWithRetry("/nonexistent/path/does-not-exist.csv", ".csv") }()

	// Let it enter the backoff wait before closing stop.
	time.Sleep(streamReopenBackoff / 2)
	close(sf.stop)

	select {
	case src := <-done:
		if src != nil {
			t.Fatal("openWithRetry returned a non-nil rowSource, want nil once stop is closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openWithRetry did not return after stop was closed")
	}
}
