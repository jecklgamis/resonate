package generator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

// newKafka starts an in-process broker with one topic of the given partition count.
func newKafka(t *testing.T, topic string, partitions int32) (*kfake.Cluster, []string) {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(partitions, topic))
	if err != nil {
		t.Fatalf("kfake: %v", err)
	}
	t.Cleanup(c.Close)
	return c, c.ListenAddrs()
}

func produce(t *testing.T, brokers []string, topic string, recs ...*kgo.Record) {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, r := range recs {
		r.Topic = topic
	}
	if err := cl.ProduceSync(context.Background(), recs...).FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}
}

func jsonRec(s string) *kgo.Record { return &kgo.Record{Value: []byte(s)} }

func nextN(f *Feeder, n int) []map[string]string {
	out := make([]map[string]string, n)
	for i := range out {
		out[i] = f.Next()
	}
	return out
}

func TestKafkaFeederJSONRowsAndMetadataWrapAround(t *testing.T) {
	_, brokers := newKafka(t, "users", 1)
	produce(t, brokers, "users",
		&kgo.Record{Key: []byte("k1"), Value: []byte(`{"id":1,"name":"alice","tags":["a"],"ok":true}`), Headers: []kgo.RecordHeader{{Key: "tenant", Value: []byte("acme")}}},
		jsonRec(`{"id":2,"name":"bob"}`),
		jsonRec(`{"id":3,"name":"carol"}`),
	)
	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "users"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows := nextN(f, 7) // wraps twice around 3 messages
	var names []string
	for _, r := range rows {
		names = append(names, r["name"])
	}
	if got, want := strings.Join(names, ","), "alice,bob,carol,alice,bob,carol,alice"; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
	r := rows[0]
	for col, want := range map[string]string{
		"id": "1", "tags": `["a"]`, "ok": "true", "_key": "k1", "_partition": "0", "_offset": "0", "_header_tenant": "acme",
	} {
		if r[col] != want {
			t.Errorf("row[%q] = %q, want %q", col, r[col], want)
		}
	}
	if !strings.HasPrefix(r["_value"], `{"id":1`) || r["_timestamp"] == "" {
		t.Errorf("bad _value/_timestamp: %v", r)
	}
}

func TestKafkaFeederReadsEveryPartition(t *testing.T) {
	_, brokers := newKafka(t, "t", 3)
	var recs []*kgo.Record
	for i := 0; i < 9; i++ {
		recs = append(recs, &kgo.Record{Partition: int32(i % 3), Value: []byte(fmt.Sprintf(`{"n":%d}`, i))})
	}
	produce(t, brokers, "t", recs...)
	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[string]bool{}
	for _, r := range nextN(f, 9) {
		seen[r["n"]] = true
	}
	if len(seen) != 9 {
		t.Errorf("saw %d distinct messages in first 9 rows, want 9: %v", len(seen), seen)
	}
}

func TestKafkaFeederRawFormatAndSkipsBadJSON(t *testing.T) {
	_, brokers := newKafka(t, "t", 1)
	produce(t, brokers, "t", jsonRec(`not json`), jsonRec(`{"id":"good"}`))

	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range nextN(f, 3) {
		if r["id"] != "good" {
			t.Errorf("row = %v, want only the valid message", r)
		}
	}
	f.Close()

	raw, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t", Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if r := raw.Next(); r["_value"] != "not json" {
		t.Errorf("raw first row = %v", r)
	}
}

func TestKafkaFeederWaitModeSeesNewMessages(t *testing.T) {
	_, brokers := newKafka(t, "t", 1)
	produce(t, brokers, "t", jsonRec(`{"v":"old"}`))
	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t", OnEnd: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if r := f.Next(); r["v"] != "old" {
		t.Fatalf("first = %v", r)
	}
	produce(t, brokers, "t", jsonRec(`{"v":"new"}`))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := f.Next(); r["v"] == "new" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("never saw the newly produced message")
		}
	}
}

func TestKafkaFeederFromEndIgnoresExistingAndIdleReturnsNil(t *testing.T) {
	_, brokers := newKafka(t, "t", 1)
	produce(t, brokers, "t", jsonRec(`{"v":"old"}`))
	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t", From: "end"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	if r := f.Next(); r != nil { // nothing new, nothing seen before: must not hang
		t.Fatalf("Next() = %v, want nil on an idle topic", r)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("idle Next() took %s", d)
	}
	produce(t, brokers, "t", jsonRec(`{"v":"fresh"}`))
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := f.Next()
		if r["v"] == "old" {
			t.Fatal("from: end delivered a pre-existing message")
		}
		if r["v"] == "fresh" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("never saw the fresh message")
		}
	}
}

func TestKafkaFeederIdleWaitModeRepeatsLastRow(t *testing.T) {
	_, brokers := newKafka(t, "t", 1)
	produce(t, brokers, "t", jsonRec(`{"v":"only"}`))
	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t", OnEnd: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < 2; i++ { // second call finds the topic idle
		if r := f.Next(); r["v"] != "only" {
			t.Fatalf("call %d = %v", i, r)
		}
	}
}

func TestKafkaFeederFailsFastOnBadSetup(t *testing.T) {
	_, brokers := newKafka(t, "t", 1)
	cases := []struct {
		name string
		opts KafkaFeederOptions
		want string
	}{
		{"no brokers", KafkaFeederOptions{Topic: "t"}, "brokers is required"},
		{"no topic", KafkaFeederOptions{Brokers: brokers}, "topic is required"},
		{"bad format", KafkaFeederOptions{Brokers: brokers, Topic: "t", Format: "xml"}, "invalid format"},
		{"bad from", KafkaFeederOptions{Brokers: brokers, Topic: "t", From: "middle"}, "invalid from"},
		{"bad on_end", KafkaFeederOptions{Brokers: brokers, Topic: "t", OnEnd: "stop"}, "invalid on_end"},
		{"wrap from end", KafkaFeederOptions{Brokers: brokers, Topic: "t", From: "end", OnEnd: "wrap"}, "needs from"},
		{"bad sasl", KafkaFeederOptions{Brokers: brokers, Topic: "t", SASLMechanism: "KERBEROS"}, "invalid sasl_mechanism"},
		{"creds without mechanism", KafkaFeederOptions{Brokers: brokers, Topic: "t", Username: "u"}, "need a sasl_mechanism"},
		{"empty topic", KafkaFeederOptions{Brokers: brokers, Topic: "t"}, "has no messages"},
		{"missing topic", KafkaFeederOptions{Brokers: brokers, Topic: "nope", Timeout: 3 * time.Second}, "nope"},
		{"unreachable", KafkaFeederOptions{Brokers: []string{"127.0.0.1:1"}, Topic: "t", Timeout: time.Second}, "can't reach brokers"},
		{"nothing parses", KafkaFeederOptions{Brokers: brokers, Topic: "bad", Timeout: time.Second}, ""},
	}
	// "nothing parses" needs a topic whose only message is not JSON.
	_, bad := newKafka(t, "bad", 1)
	produce(t, bad, "bad", jsonRec("nope"))
	cases[len(cases)-1].opts.Brokers = bad
	cases[len(cases)-1].want = "no usable message"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewKafkaFeeder(tc.opts)
			if err == nil {
				f.Close()
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestKafkaFeederConcurrentNextAndCloseDoNotLeak(t *testing.T) {
	_, brokers := newKafka(t, "t", 1)
	produce(t, brokers, "t", jsonRec(`{"v":"1"}`), jsonRec(`{"v":"2"}`))
	f, err := NewKafkaFeeder(KafkaFeederOptions{Brokers: brokers, Topic: "t"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				if f.Next() == nil {
					t.Error("nil row")
				}
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	closed := make(chan struct{})
	go func() { f.Close(); f.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() hung")
	}
}
