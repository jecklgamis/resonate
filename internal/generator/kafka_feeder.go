package generator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// KafkaFeederOptions configures NewKafkaFeeder: a Feeder whose rows are the
// messages of a Kafka topic. The topic is consumed directly from its
// partitions — no consumer group, no committed offsets — so running a test
// never disturbs (or is disturbed by) anyone else's consumers.
type KafkaFeederOptions struct {
	// Brokers are the seed brokers, "host:port". Required.
	Brokers []string
	// Topic to consume. Required.
	Topic string

	// Format of each message value: "json" (default; the value must be a
	// JSON object, whose top-level fields become columns) or "raw" (only
	// the _-prefixed metadata columns). Messages that don't parse as
	// "json" are skipped.
	Format string

	// From is where to start: "start" (default; the oldest retained
	// message) or "end" (only messages produced after construction).
	From string

	// OnEnd is what to do on reaching the last message: "wrap" (re-read
	// the topic from the start, so the rows cycle like a file feeder's;
	// the default for From "start") or "wait" (keep consuming newly
	// produced messages; the default for From "end"). "wrap" with From
	// "end" is rejected: there'd be nothing to wrap around.
	OnEnd string

	// Timeout bounds connecting and, when the topic must already hold
	// data, waiting for the first message. Zero means 10s.
	Timeout time.Duration

	// TLS options, same meaning as on HTTPOptions. TLS is used if any is
	// set, or if TLS is true.
	TLS      bool
	Insecure bool
	CAFile   string
	CertFile string
	KeyFile  string

	// SASL: "" (none), "PLAIN", "SCRAM-SHA-256" or "SCRAM-SHA-512".
	SASLMechanism string
	Username      string
	Password      string
}

// Column names every Kafka row carries, in addition to the message's own
// JSON fields (when Format is "json"). They start with "_" so they can't
// collide with a column of a flat JSON object by accident; if one does, the
// metadata wins.
const (
	KafkaColKey       = "_key"
	KafkaColValue     = "_value"
	KafkaColPartition = "_partition"
	KafkaColOffset    = "_offset"
	KafkaColTimestamp = "_timestamp" // Unix milliseconds
	kafkaColHeader    = "_header_"   // + header name
)

const (
	kafkaDefaultTimeout = 10 * time.Second
	// kafkaIdleWait is how long Next() waits for a message before handing
	// back the most recent row instead. Without it, a worker would block
	// forever on an idle topic and the run could never end.
	kafkaIdleWait    = time.Second
	kafkaRetryPeriod = 200 * time.Millisecond
)

// kafkaFeeder is the Feeder source behind NewKafkaFeeder. Like streamFeeder
// it is a background goroutine filling a bounded channel
// (streamFeederBufferSize rows), so memory doesn't depend on the topic's
// size, and the same Close() precondition applies: nothing may still be
// calling next().
type kafkaFeeder struct {
	opts KafkaFeederOptions
	rows chan map[string]string
	last atomic.Pointer[map[string]string]

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	firstOnce sync.Once
	first     chan struct{} // closed once the first row is available
}

// NewKafkaFeeder connects to the brokers and returns a Feeder over opts.Topic.
// It fails fast — before any request is sent — if the brokers are
// unreachable, the topic doesn't exist, the options are invalid, or (unless
// tailing from the end) the topic holds no messages.
func NewKafkaFeeder(opts KafkaFeederOptions) (*Feeder, error) {
	if len(opts.Brokers) == 0 {
		return nil, errors.New("kafka feeder: brokers is required")
	}
	if opts.Topic == "" {
		return nil, errors.New("kafka feeder: topic is required")
	}
	switch opts.Format {
	case "":
		opts.Format = "json"
	case "json", "raw":
	default:
		return nil, fmt.Errorf("kafka feeder: invalid format %q (want \"json\" or \"raw\")", opts.Format)
	}
	switch opts.From {
	case "":
		opts.From = "start"
	case "start", "end":
	default:
		return nil, fmt.Errorf("kafka feeder: invalid from %q (want \"start\" or \"end\")", opts.From)
	}
	switch opts.OnEnd {
	case "":
		if opts.From == "end" {
			opts.OnEnd = "wait"
		} else {
			opts.OnEnd = "wrap"
		}
	case "wrap", "wait":
	default:
		return nil, fmt.Errorf("kafka feeder: invalid on_end %q (want \"wrap\" or \"wait\")", opts.OnEnd)
	}
	if opts.From == "end" && opts.OnEnd == "wrap" {
		return nil, errors.New("kafka feeder: on_end \"wrap\" needs from \"start\" (from \"end\" has no messages to wrap around)")
	}
	if opts.Timeout == 0 {
		opts.Timeout = kafkaDefaultTimeout
	}
	if _, err := kafkaSASL(opts); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	kf := &kafkaFeeder{
		opts:   opts,
		rows:   make(chan map[string]string, streamFeederBufferSize),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		first:  make(chan struct{}),
	}

	// First pass is set up synchronously so connection/topic errors surface here.
	cl, ends, err := kf.open()
	if err != nil {
		cancel()
		return nil, err
	}
	if opts.From == "start" && len(ends) == 0 {
		cl.Close()
		cancel()
		return nil, fmt.Errorf("kafka feeder: topic %q has no messages (to wait for new ones, set from: end)", opts.Topic)
	}

	go kf.run(cl, ends)

	if opts.From == "start" {
		select {
		case <-kf.first:
		case <-time.After(opts.Timeout):
			kf.close()
			return nil, fmt.Errorf("kafka feeder: no usable message from topic %q within %s (check format %q)", opts.Topic, opts.Timeout, opts.Format)
		}
	}
	return &Feeder{kafka: kf}, nil
}

func kafkaSASL(opts KafkaFeederOptions) (sasl.Mechanism, error) {
	switch strings.ToUpper(opts.SASLMechanism) {
	case "":
		if opts.Username != "" || opts.Password != "" {
			return nil, errors.New("kafka feeder: username/password need a sasl_mechanism")
		}
		return nil, nil
	case "PLAIN":
		return plain.Auth{User: opts.Username, Pass: opts.Password}.AsMechanism(), nil
	case "SCRAM-SHA-256":
		return scram.Auth{User: opts.Username, Pass: opts.Password}.AsSha256Mechanism(), nil
	case "SCRAM-SHA-512":
		return scram.Auth{User: opts.Username, Pass: opts.Password}.AsSha512Mechanism(), nil
	default:
		return nil, fmt.Errorf("kafka feeder: invalid sasl_mechanism %q (want PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512)", opts.SASLMechanism)
	}
}

// open dials the cluster and starts consuming one pass over the topic. ends
// maps each partition holding data to the offset of its last message at the
// moment of the call — the "end" that wrap mode counts to. With From "end"
// the map is nil.
func (kf *kafkaFeeder) open() (*kgo.Client, map[int32]int64, error) {
	o := kf.opts
	kopts := []kgo.Opt{
		kgo.SeedBrokers(o.Brokers...),
		kgo.DialTimeout(o.Timeout),
		kgo.RequestRetries(2),
		kgo.ClientID("resonate"),
	}
	if o.TLS || o.Insecure || o.CAFile != "" || o.CertFile != "" || o.KeyFile != "" {
		cfg, err := newTLSConfig(o.Insecure, o.CAFile, o.CertFile, o.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("kafka feeder: %w", err)
		}
		kopts = append(kopts, kgo.DialTLSConfig(cfg))
	}
	if mech, _ := kafkaSASL(o); mech != nil {
		kopts = append(kopts, kgo.SASL(mech))
	}
	cl, err := kgo.NewClient(kopts...)
	if err != nil {
		return nil, nil, fmt.Errorf("kafka feeder: %w", err)
	}

	ctx, cancel := context.WithTimeout(kf.ctx, o.Timeout)
	defer cancel()

	// Partitions of the topic.
	mreq := kmsg.NewPtrMetadataRequest()
	mt := kmsg.NewMetadataRequestTopic()
	mt.Topic = kmsg.StringPtr(o.Topic)
	mreq.Topics = append(mreq.Topics, mt)
	mresp, err := mreq.RequestWith(ctx, cl)
	if err != nil {
		cl.Close()
		return nil, nil, fmt.Errorf("kafka feeder: can't reach brokers %v: %w", o.Brokers, err)
	}
	var parts []int32
	for _, t := range mresp.Topics {
		if t.Topic == nil || *t.Topic != o.Topic {
			continue
		}
		if err := kerrToErr(t.ErrorCode); err != nil {
			cl.Close()
			return nil, nil, fmt.Errorf("kafka feeder: topic %q: %w", o.Topic, err)
		}
		for _, p := range t.Partitions {
			parts = append(parts, p.Partition)
		}
	}
	if len(parts) == 0 {
		cl.Close()
		return nil, nil, fmt.Errorf("kafka feeder: topic %q has no partitions", o.Topic)
	}

	starts, err := kafkaOffsets(ctx, cl, o.Topic, parts, -2)
	if err != nil {
		cl.Close()
		return nil, nil, err
	}
	endOffs, err := kafkaOffsets(ctx, cl, o.Topic, parts, -1)
	if err != nil {
		cl.Close()
		return nil, nil, err
	}

	assign := map[int32]kgo.Offset{}
	var ends map[int32]int64
	if o.From == "start" {
		ends = map[int32]int64{}
		for _, p := range parts {
			if endOffs[p] > starts[p] {
				assign[p] = kgo.NewOffset().At(starts[p])
				ends[p] = endOffs[p] - 1
			}
		}
	} else {
		for _, p := range parts {
			assign[p] = kgo.NewOffset().At(endOffs[p])
		}
	}
	cl.AddConsumePartitions(map[string]map[int32]kgo.Offset{o.Topic: assign})
	return cl, ends, nil
}

func kerrToErr(code int16) error {
	if code == 0 {
		return nil
	}
	return kerr.ErrorForCode(code)
}

// kafkaOffsets asks for each partition's offset at timestamp ts (-2 =
// earliest, -1 = latest/high watermark).
func kafkaOffsets(ctx context.Context, cl *kgo.Client, topic string, parts []int32, ts int64) (map[int32]int64, error) {
	req := kmsg.NewPtrListOffsetsRequest()
	req.ReplicaID = -1
	rt := kmsg.NewListOffsetsRequestTopic()
	rt.Topic = topic
	for _, p := range parts {
		rp := kmsg.NewListOffsetsRequestTopicPartition()
		rp.Partition = p
		rp.Timestamp = ts
		rt.Partitions = append(rt.Partitions, rp)
	}
	req.Topics = append(req.Topics, rt)
	resp, err := req.RequestWith(ctx, cl)
	if err != nil {
		return nil, fmt.Errorf("kafka feeder: listing offsets of %q: %w", topic, err)
	}
	out := map[int32]int64{}
	for _, t := range resp.Topics {
		for _, p := range t.Partitions {
			if err := kerrToErr(p.ErrorCode); err != nil {
				return nil, fmt.Errorf("kafka feeder: listing offsets of %q partition %d: %w", topic, p.Partition, err)
			}
			out[p.Partition] = p.Offset
		}
	}
	return out, nil
}

// run consumes cl until close(). In wrap mode, once every partition has been
// read up to its recorded end it closes the client and starts a fresh pass.
func (kf *kafkaFeeder) run(cl *kgo.Client, ends map[int32]int64) {
	defer close(kf.done)
	wrap := kf.opts.OnEnd == "wrap"

	for {
		remaining := make(map[int32]int64, len(ends))
		for p, e := range ends {
			remaining[p] = e
		}

	pass:
		for kf.ctx.Err() == nil {
			fetches := cl.PollFetches(kf.ctx)
			if kf.ctx.Err() != nil {
				break
			}
			if fetches.IsClientClosed() {
				return
			}
			if errs := fetches.Errors(); len(errs) > 0 && fetches.NumRecords() == 0 {
				// kgo retries internally; just don't spin if it keeps failing.
				select {
				case <-time.After(kafkaRetryPeriod):
				case <-kf.ctx.Done():
				}
			}
			var stopped bool
			fetches.EachRecord(func(r *kgo.Record) {
				if stopped {
					return
				}
				if wrap {
					end, ok := remaining[r.Partition]
					if !ok {
						return // beyond this pass's snapshot
					}
					if r.Offset >= end {
						delete(remaining, r.Partition)
					}
				}
				if row, err := kafkaRow(r, kf.opts.Format); err == nil {
					select {
					case kf.rows <- row:
						kf.firstOnce.Do(func() { close(kf.first) })
					case <-kf.ctx.Done():
						stopped = true
					}
				}
			})
			if wrap && len(remaining) == 0 {
				break pass
			}
		}
		cl.Close()
		if kf.ctx.Err() != nil || !wrap {
			return
		}

		// Next pass: reconnect, recompute the end of each partition.
		for {
			ncl, nends, err := kf.open()
			if err == nil && len(nends) > 0 {
				cl, ends = ncl, nends
				break
			}
			if err == nil {
				ncl.Close() // topic emptied (retention); keep trying
			}
			select {
			case <-time.After(kafkaRetryPeriod):
			case <-kf.ctx.Done():
				return
			}
		}
	}
}

// kafkaRow turns a message into a feeder row.
func kafkaRow(r *kgo.Record, format string) (map[string]string, error) {
	row := map[string]string{}
	if format == "json" {
		var obj map[string]any
		if err := json.Unmarshal(r.Value, &obj); err != nil {
			return nil, err
		}
		for k, v := range obj {
			row[k] = stringifyJSONValue(v)
		}
	}
	row[KafkaColKey] = string(r.Key)
	row[KafkaColValue] = string(r.Value)
	row[KafkaColPartition] = strconv.Itoa(int(r.Partition))
	row[KafkaColOffset] = strconv.FormatInt(r.Offset, 10)
	row[KafkaColTimestamp] = strconv.FormatInt(r.Timestamp.UnixMilli(), 10)
	for _, h := range r.Headers {
		row[kafkaColHeader+h.Key] = string(h.Value)
	}
	return row, nil
}

// next returns the next message's row. If the topic is idle for
// kafkaIdleWait it returns the most recent row again (or nil if there has
// never been one), so a quiet topic slows the test down rather than
// hanging it.
func (kf *kafkaFeeder) next() map[string]string {
	select {
	case r := <-kf.rows:
		kf.last.Store(&r)
		return r
	default:
	}
	t := time.NewTimer(kafkaIdleWait)
	defer t.Stop()
	select {
	case r := <-kf.rows:
		kf.last.Store(&r)
		return r
	case <-t.C:
		if p := kf.last.Load(); p != nil {
			return *p
		}
		return nil
	}
}

func (kf *kafkaFeeder) close() {
	kf.cancel()
	<-kf.done
}
