// Package observer reads the audit trail that the observer service serves.
//
// The observer is on NEITHER of the lab's two networks — it reaches the shared
// audit-logs volume without becoming a channel between the secure side and the
// lab side — and it publishes on the host's loopback only, over plain HTTP with
// no auth. So this is a host-side reader of a host-local URL, which is the only
// shape in which reading the trail is safe.
//
// Nothing here knows what a provider is. An audit event is whatever JSON the
// broker, proxy or cred-gateway wrote, and it is rendered by shape — the three
// fields every writer agrees on, then whatever else the line carried. A
// formatter that recognised particular providers would be exactly the
// per-provider code this repo does not have.
package observer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EventsPath is where the observer serves the trail as server-sent events.
// Its `/` is the dashboard `sal observer open` points a browser at, and this
// is the same data for a terminal that has no browser at all.
const EventsPath = "/events"

// DefaultIdle is how long a quiet stream means "that was all of it".
//
// The observer replays a backlog to every client that connects and then holds
// the connection open, and it sends no marker between the two. So --follow=false
// cannot be implemented by reading to a boundary, because there is none: what
// has already happened is defined as what arrived before the stream went quiet.
// On loopback the backlog lands in one burst, so this only has to outlast a
// scheduling hiccup.
const DefaultIdle = 500 * time.Millisecond

// maxEventBytes bounds one event. The observer already truncates the one field
// it synthesises, but a writer could emit a very long line and a tail that
// tried to buffer it without limit would be a way to make sal consume memory.
const maxEventBytes = 1 << 20

// ErrClosed means the observer hung up while we were following it, which
// generally means its container went away. It is an error rather than a clean
// end: a tail that exits 0 when the thing it was watching disappeared tells a
// script the wrong thing.
var ErrClosed = errors.New("the observer closed the stream")

// Options controls one Stream call.
type Options struct {
	// Follow keeps reading until the stream ends or the context is cancelled.
	// When false, Stream returns once the stream has been quiet for Idle.
	Follow bool

	// Idle is the quiet period that ends a non-following read. Zero means
	// DefaultIdle.
	Idle time.Duration

	// Client is for tests. A nil Client gets one with NO timeout, which is
	// deliberate — this connection is meant to stay open.
	Client *http.Client
}

// Stream writes the observer's audit events to w, one formatted line each, and
// reports how many it wrote.
func Stream(ctx context.Context, baseURL string, w io.Writer, opts Options) (int, error) {
	if opts.Idle <= 0 {
		opts.Idle = DefaultIdle
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{}
	}

	url := strings.TrimSuffix(baseURL, "/") + EventsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach the observer at %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the observer answered %s at %s", resp.Status, url)
	}

	events := make(chan []byte)
	scanErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go readEvents(resp.Body, events, scanErr, done)

	// The idle timer runs only for a non-following read. Started at connect
	// rather than at the first event, so a lab that has done nothing yet
	// returns promptly with no output instead of waiting for a first line
	// that may never come.
	timer := time.NewTimer(opts.Idle)
	defer timer.Stop()
	var idle <-chan time.Time
	if !opts.Follow {
		idle = timer.C
	}

	n := 0
	for {
		select {
		case <-ctx.Done():
			return n, ctx.Err()

		case <-idle:
			return n, nil

		case raw, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return n, ctx.Err()
				}
				if err := <-scanErr; err != nil {
					return n, fmt.Errorf("reading the observer's stream: %w", err)
				}
				if opts.Follow {
					return n, ErrClosed
				}
				return n, nil
			}
			if _, err := fmt.Fprintln(w, Format(raw)); err != nil {
				return n, err
			}
			n++
			if !opts.Follow {
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(opts.Idle)
			}
		}
	}
}

var dataPrefix = []byte("data:")

// readEvents turns the SSE framing into one payload per event.
//
// Only `data:` is honoured. The observer sends nothing else, and a reader that
// acted on `event:` or `retry:` would be implementing a protocol neither end
// is speaking.
func readEvents(body io.Reader, events chan<- []byte, scanErr chan<- error, done <-chan struct{}) {
	defer close(events)

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventBytes)

	var data []byte
	emit := func() bool {
		if len(data) == 0 {
			return true
		}
		select {
		case events <- append([]byte(nil), data...):
			data = data[:0]
			return true
		case <-done:
			return false
		}
	}

	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if !emit() {
				return
			}
		case bytes.HasPrefix(line, dataPrefix):
			chunk := bytes.TrimPrefix(line, dataPrefix)
			chunk = bytes.TrimPrefix(chunk, []byte(" "))
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
	}
	// A stream that ends mid-frame still had an event in it.
	if sc.Err() == nil {
		emit()
	}
	scanErr <- sc.Err()
}

// The three fields every writer in the stack agrees on: broker/audit.js,
// proxy/audit.py and cred-gateway's nginx log_format all emit ts, service and
// event, then whatever the line is about. Anything else is rendered as k=v, so
// a writer that adds a field shows it without a change here.
const (
	tsWidth      = 26
	serviceWidth = 14
	eventWidth   = 22
)

// Format renders one audit event as a single line.
func Format(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var ev map[string]any
	if err := dec.Decode(&ev); err != nil {
		// Whatever this is, it was in the trail. Passing it through unread is
		// better than dropping it: the observer itself does the same with a
		// line it could not parse.
		return strings.TrimSpace(string(raw))
	}

	var b strings.Builder
	ts, hasTS := ev["ts"].(string)
	service, hasService := ev["service"].(string)
	event, hasEvent := ev["event"].(string)
	if hasTS || hasService || hasEvent {
		pad(&b, ts, tsWidth)
		pad(&b, service, serviceWidth)
		pad(&b, event, eventWidth)
	}

	keys := make([]string, 0, len(ev))
	for k := range ev {
		if k == "ts" || k == "service" || k == "event" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s", k, value(ev[k]))
	}

	return strings.TrimRight(b.String(), " ")
}

// pad keeps the columns lined up without ever truncating. A trail is read to
// find out what happened, and a cut-off value is the one thing a reader cannot
// recover from the line they are looking at.
func pad(b *strings.Builder, s string, width int) {
	b.WriteString(s)
	if n := width - len(s); n > 0 {
		b.WriteString(strings.Repeat(" ", n))
	} else {
		b.WriteByte(' ')
	}
}

func value(v any) string {
	switch t := v.(type) {
	case string:
		// Quoted only when it would otherwise run into the next field, so the
		// common case stays greppable.
		if strings.ContainsAny(t, " \t\"") {
			return strconv.Quote(t)
		}
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return "null"
	default:
		encoded, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(encoded)
	}
}
