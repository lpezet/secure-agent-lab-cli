package observer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer serves the given payloads as one SSE frame each. It holds the
// connection open afterwards unless closeAfter is set, which is the observer's
// own behaviour: a backlog replay with no marker where it ends.
func sseServer(t *testing.T, closeAfter bool, payloads ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EventsPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, p := range payloads {
			fmt.Fprintf(w, "data: %s\n\n", p)
		}
		w.(http.Flusher).Flush()
		if closeAfter {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamPrintsTheBacklogAndStops(t *testing.T) {
	srv := sseServer(t, false,
		`{"ts":"2026-08-15T09:00:00+00:00","service":"proxy","event":"request_allowed","host":"api.example.test"}`,
		`{"ts":"2026-08-15T09:00:01+00:00","service":"broker","event":"cred_issued","provider":"acme"}`,
	)

	var out bytes.Buffer
	n, err := Stream(context.Background(), srv.URL, &out, Options{Idle: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if n != 2 {
		t.Errorf("wrote %d events, want 2", n)
	}

	// A stream with no end marker still has to terminate for --follow=false,
	// and it does so on quiet rather than on a boundary that does not exist.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "request_allowed") || !strings.Contains(lines[0], "host=api.example.test") {
		t.Errorf("first line lost its content: %q", lines[0])
	}
}

func TestStreamFollowReportsAClosedStream(t *testing.T) {
	srv := sseServer(t, true, `{"ts":"t","service":"proxy","event":"one"}`)

	var out bytes.Buffer
	n, err := Stream(context.Background(), srv.URL, &out, Options{Follow: true})

	// The observer hanging up means its container went away. Exiting 0 there
	// would tell a script that a tail ended normally when what it watched
	// disappeared.
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if n != 1 {
		t.Errorf("wrote %d events, want the one that arrived before the close", n)
	}
}

func TestStreamStopsWhenTheContextIsCancelled(t *testing.T) {
	srv := sseServer(t, false, `{"ts":"t","service":"proxy","event":"one"}`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var out bytes.Buffer
	_, err := Stream(ctx, srv.URL, &out, Options{Follow: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStreamRefusesAnUnexpectedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	if _, err := Stream(context.Background(), srv.URL, &bytes.Buffer{}, Options{Idle: 50 * time.Millisecond}); err == nil {
		t.Fatal("a non-200 answer is not a trail, and must not look like an empty one")
	}
}

func TestStreamJoinsMultiLineFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// SSE splits a payload containing newlines across several data lines.
		// The observer does not do this today; a reader that could not
		// reassemble it would corrupt the trail if it ever did.
		fmt.Fprint(w, "data: {\"service\":\"proxy\",\n")
		fmt.Fprint(w, "data: \"event\":\"split\"}\n\n")
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	if _, err := Stream(context.Background(), srv.URL, &out, Options{Idle: 50 * time.Millisecond}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(out.String(), "split") {
		t.Errorf("frame was not reassembled: %q", out.String())
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "the three fields every writer agrees on lead, the rest follow sorted",
			raw:  `{"event":"request","service":"cred-gateway","ts":"2026-08-15T09:00:00+00:00","status":403,"path":"/acme/token","method":"POST"}`,
			want: []string{"2026-08-15T09:00:00+00:00", "cred-gateway", "request", "method=POST path=/acme/token status=403"},
		},
		{
			name: "a big number keeps every digit",
			raw:  `{"service":"broker","event":"x","bytes":9007199254740993}`,
			want: []string{"bytes=9007199254740993"},
		},
		{
			name: "a value with spaces is quoted so it cannot be read as two fields",
			raw:  `{"service":"observer","event":"unparseable_line","raw":"not json at all"}`,
			want: []string{`raw="not json at all"`},
		},
		{
			name: "a nested value is kept, compactly",
			raw:  `{"service":"proxy","event":"x","headers":{"accept":"*/*"}}`,
			want: []string{`headers={"accept":"*/*"}`},
		},
		{
			name: "something that is not an event object is passed through rather than dropped",
			raw:  `this was never JSON`,
			want: []string{"this was never JSON"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Format([]byte(c.raw))
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("Format(%s)\n = %q\nwant it to contain %q", c.raw, got, want)
				}
			}
		})
	}
}

// The formatter renders whatever the line carried. A provider name reaching it
// is data, never something this repo has heard of — the same rule that keeps
// per-provider code out of the installer.
func TestFormatDoesNotInterpretTheFields(t *testing.T) {
	got := Format([]byte(`{"service":"broker","event":"cred_issued","provider":"whatever-they-called-it"}`))
	if !strings.Contains(got, "provider=whatever-they-called-it") {
		t.Errorf("Format dropped or rewrote an unknown field: %q", got)
	}
}
