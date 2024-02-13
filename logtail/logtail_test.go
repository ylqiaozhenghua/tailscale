// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package logtail

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/tstest"
	"tailscale.com/tstime"
)

func TestFastShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	testServ := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {}))
	defer testServ.Close()

	l := NewLogger(Config{
		BaseURL: testServ.URL,
	}, t.Logf)
	err := l.Shutdown(ctx)
	if err != nil {
		t.Error(err)
	}
}

// maximum number of times a test will call l.Write()
const logLines = 3

type LogtailTestServer struct {
	srv      *httptest.Server // Log server
	uploaded chan []byte
}

func NewLogtailTestHarness(t *testing.T) (*LogtailTestServer, *Logger) {
	ts := LogtailTestServer{}

	// max channel backlog = 1 "started" + #logLines x "log line" + 1 "closed"
	ts.uploaded = make(chan []byte, 2+logLines)

	ts.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error("failed to read HTTP request")
			}
			ts.uploaded <- body
		}))

	t.Cleanup(ts.srv.Close)

	l := NewLogger(Config{BaseURL: ts.srv.URL}, t.Logf)

	// There is always an initial "logtail started" message
	body := <-ts.uploaded
	if !strings.Contains(string(body), "started") {
		t.Errorf("unknown start logging statement: %q", string(body))
	}

	return &ts, l
}

func TestDrainPendingMessages(t *testing.T) {
	ts, l := NewLogtailTestHarness(t)

	for i := 0; i < logLines; i++ {
		l.Write([]byte("log line"))
	}

	// all of the "log line" messages usually arrive at once, but poll if needed.
	body := ""
	for i := 0; i <= logLines; i++ {
		body += string(<-ts.uploaded)
		count := strings.Count(body, "log line")
		if count == logLines {
			break
		}
		// if we never find count == logLines, the test will eventually time out.
	}

	err := l.Shutdown(context.Background())
	if err != nil {
		t.Error(err)
	}
}

func TestEncodeAndUploadMessages(t *testing.T) {
	ts, l := NewLogtailTestHarness(t)

	tests := []struct {
		name string
		log  string
		want string
	}{
		{
			"plain text",
			"log line",
			"log line",
		},
		{
			"simple JSON",
			`{"text": "log line"}`,
			"log line",
		},
	}

	for _, tt := range tests {
		io.WriteString(l, tt.log)
		body := <-ts.uploaded

		data := unmarshalOne(t, body)
		got := data["text"]
		if got != tt.want {
			t.Errorf("%s: got %q; want %q", tt.name, got.(string), tt.want)
		}

		ltail, ok := data["logtail"]
		if ok {
			logtailmap := ltail.(map[string]any)
			_, ok = logtailmap["client_time"]
			if !ok {
				t.Errorf("%s: no client_time present", tt.name)
			}
		} else {
			t.Errorf("%s: no logtail map present", tt.name)
		}
	}

	err := l.Shutdown(context.Background())
	if err != nil {
		t.Error(err)
	}
}

func TestEncodeSpecialCases(t *testing.T) {
	ts, l := NewLogtailTestHarness(t)

	// -------------------------------------------------------------------------

	// JSON log message already contains a logtail field.
	io.WriteString(l, `{"logtail": "LOGTAIL", "text": "text"}`)
	body := <-ts.uploaded
	data := unmarshalOne(t, body)
	errorHasLogtail, ok := data["error_has_logtail"]
	if ok {
		if errorHasLogtail != "LOGTAIL" {
			t.Errorf("error_has_logtail: got:%q; want:%q",
				errorHasLogtail, "LOGTAIL")
		}
	} else {
		t.Errorf("no error_has_logtail field: %v", data)
	}

	// -------------------------------------------------------------------------

	// special characters
	io.WriteString(l, "\b\f\n\r\t"+`"\`)
	bodytext := string(<-ts.uploaded)
	// json.Unmarshal would unescape the characters, we have to look at the encoded text
	escaped := strings.Contains(bodytext, `\b\f\n\r\t\"\`)
	if !escaped {
		t.Errorf("special characters got %s", bodytext)
	}

	// -------------------------------------------------------------------------

	// skipClientTime to omit the logtail metadata
	l.skipClientTime = true
	io.WriteString(l, "text")
	body = <-ts.uploaded
	data = unmarshalOne(t, body)
	_, ok = data["logtail"]
	if ok {
		t.Errorf("skipClientTime: unexpected logtail map present: %v", data)
	}

	// -------------------------------------------------------------------------

	// lowMem + long string
	l.skipClientTime = false
	l.lowMem = true
	longStr := strings.Repeat("0", 5120)
	io.WriteString(l, longStr)
	body = <-ts.uploaded
	data = unmarshalOne(t, body)
	text, ok := data["text"]
	if !ok {
		t.Errorf("lowMem: no text %v", data)
	}
	if n := len(text.(string)); n > 4500 {
		t.Errorf("lowMem: got %d chars; want <4500 chars", n)
	}

	// -------------------------------------------------------------------------

	err := l.Shutdown(context.Background())
	if err != nil {
		t.Error(err)
	}
}

var sink []byte

func TestLoggerEncodeTextAllocs(t *testing.T) {
	lg := &Logger{clock: tstime.StdClock{}}
	inBuf := []byte("some text to encode")
	procID := uint32(0x24d32ee9)
	procSequence := uint64(0x12346)
	err := tstest.MinAllocsPerRun(t, 1, func() {
		sink = lg.encodeText(inBuf, false, procID, procSequence, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoggerWriteLength(t *testing.T) {
	lg := &Logger{
		clock:  tstime.StdClock{},
		buffer: NewMemoryBuffer(1024),
	}
	inBuf := []byte("some text to encode")
	n, err := lg.Write(inBuf)
	if err != nil {
		t.Error(err)
	}
	if n != len(inBuf) {
		t.Errorf("logger.Write wrote %d bytes, expected %d", n, len(inBuf))
	}
}

func TestParseAndRemoveLogLevel(t *testing.T) {
	tests := []struct {
		log       string
		wantLevel int
		wantLog   string
	}{
		{
			"no level",
			0,
			"no level",
		},
		{
			"[v1] level 1",
			1,
			"level 1",
		},
		{
			"level 1 [v1] ",
			1,
			"level 1 ",
		},
		{
			"[v2] level 2",
			2,
			"level 2",
		},
		{
			"level [v2] 2",
			2,
			"level 2",
		},
		{
			"[v3] no level 3",
			0,
			"[v3] no level 3",
		},
		{
			"some ignored text then [v\x00JSON]5{\"foo\":1234}",
			5,
			`{"foo":1234}`,
		},
	}

	for _, tt := range tests {
		gotLevel, gotLog := parseAndRemoveLogLevel([]byte(tt.log))
		if gotLevel != tt.wantLevel {
			t.Errorf("parseAndRemoveLogLevel(%q): got:%d; want %d",
				tt.log, gotLevel, tt.wantLevel)
		}
		if string(gotLog) != tt.wantLog {
			t.Errorf("parseAndRemoveLogLevel(%q): got:%q; want %q",
				tt.log, gotLog, tt.wantLog)
		}
	}
}

func unmarshalOne(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var entries []map[string]any
	err := json.Unmarshal(body, &entries)
	if err != nil {
		t.Error(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	return entries[0]
}

func TestEncodeTextTruncation(t *testing.T) {
	lg := &Logger{clock: tstime.StdClock{}, lowMem: true}
	in := bytes.Repeat([]byte("a"), 5120)
	b := lg.encodeText(in, true, 0, 0, 0)
	got := string(b)
	want := `{"text": "` + strings.Repeat("a", 4096) + `…+1024"}` + "\n"
	if got != want {
		t.Errorf("got:\n%qwant:\n%q\n", got, want)
	}
}

type simpleMemBuf struct {
	Buffer
	buf bytes.Buffer
}

func (b *simpleMemBuf) Write(p []byte) (n int, err error) { return b.buf.Write(p) }

func TestEncode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			"normal",
			`{"logtail": {"client_time": "1970-01-01T00:02:03.000000456Z","proc_id": 7,"proc_seq": 1}, "text": "normal"}` + "\n",
		},
		{
			"and a [v1] level one",
			`{"logtail": {"client_time": "1970-01-01T00:02:03.000000456Z","proc_id": 7,"proc_seq": 1}, "v":1,"text": "and a level one"}` + "\n",
		},
		{
			"[v2] some verbose two",
			`{"logtail": {"client_time": "1970-01-01T00:02:03.000000456Z","proc_id": 7,"proc_seq": 1}, "v":2,"text": "some verbose two"}` + "\n",
		},
		{
			"{}",
			`{"logtail":{"client_time":"1970-01-01T00:02:03.000000456Z","proc_id":7,"proc_seq":1}}` + "\n",
		},
		{
			`{"foo":"bar"}`,
			`{"foo":"bar","logtail":{"client_time":"1970-01-01T00:02:03.000000456Z","proc_id":7,"proc_seq":1}}` + "\n",
		},
		{
			"foo: [v\x00JSON]0{\"foo\":1}",
			"{\"foo\":1,\"logtail\":{\"client_time\":\"1970-01-01T00:02:03.000000456Z\",\"proc_id\":7,\"proc_seq\":1}}\n",
		},
		{
			"foo: [v\x00JSON]2{\"foo\":1}",
			"{\"foo\":1,\"logtail\":{\"client_time\":\"1970-01-01T00:02:03.000000456Z\",\"proc_id\":7,\"proc_seq\":1},\"v\":2}\n",
		},
	}
	for _, tt := range tests {
		buf := new(simpleMemBuf)
		lg := &Logger{
			clock:        tstest.NewClock(tstest.ClockOpts{Start: time.Unix(123, 456).UTC()}),
			buffer:       buf,
			procID:       7,
			procSequence: 1,
		}
		io.WriteString(lg, tt.in)
		got := buf.buf.String()
		if got != tt.want {
			t.Errorf("for %q,\n got: %#q\nwant: %#q\n", tt.in, got, tt.want)
		}
		if err := json.Compact(new(bytes.Buffer), buf.buf.Bytes()); err != nil {
			t.Errorf("invalid output JSON for %q: %s", tt.in, got)
		}
	}
}

// Test that even if Logger.Write modifies the input buffer, we still return the
// length of the input buffer, not what we shrank it down to. Otherwise the
// caller will think we did a short write, violating the io.Writer contract.
func TestLoggerWriteResult(t *testing.T) {
	buf := NewMemoryBuffer(100)
	lg := &Logger{
		clock:  tstest.NewClock(tstest.ClockOpts{Start: time.Unix(123, 0)}),
		buffer: buf,
	}

	const in = "[v1] foo"
	n, err := lg.Write([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := n, len(in); got != want {
		t.Errorf("Write = %v; want %v", got, want)
	}
	back, err := buf.TryReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(back), `{"logtail": {"client_time": "1970-01-01T00:02:03Z"}, "v":1,"text": "foo"}`+"\n"; got != want {
		t.Errorf("mismatch.\n got: %#q\nwant: %#q", back, want)
	}
}
func TestRedact(t *testing.T) {
	envknob.Setenv("TS_OBSCURE_LOGGED_IPS", "true")
	tests := []struct {
		in   string
		want string
	}{
		// tests for ipv4 addresses
		{
			"120.100.30.47",
			"120.100.x.x",
		},
		{
			"192.167.0.1/65",
			"192.167.x.x/65",
		},
		{
			"node [5Btdd] d:e89a3384f526d251 now using 10.0.0.222:41641 mtu=1360 tx=d81a8a35a0ce",
			"node [5Btdd] d:e89a3384f526d251 now using 10.0.x.x:41641 mtu=1360 tx=d81a8a35a0ce",
		},
		//tests for ipv6 addresses
		{
			"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			"2001:0db8:x",
		},
		{
			"2345:0425:2CA1:0000:0000:0567:5673:23b5",
			"2345:0425:x",
		},
		{
			"2601:645:8200:edf0::c9de/64",
			"2601:645:x/64",
		},
		{
			"node [5Btdd] d:e89a3384f526d251 now using 2051:0000:140F::875B:131C mtu=1360 tx=d81a8a35a0ce",
			"node [5Btdd] d:e89a3384f526d251 now using 2051:0000:x mtu=1360 tx=d81a8a35a0ce",
		},
		{
			"2601:645:8200:edf0::c9de/64 2601:645:8200:edf0:1ce9:b17d:71f5:f6a3/64",
			"2601:645:x/64 2601:645:x/64",
		},
		//tests for tailscale ip addresses
		{
			"100.64.5.6",
			"100.64.5.6",
		},
		{
			"fd7a:115c:a1e0::/96",
			"fd7a:115c:a1e0::/96",
		},
		//tests for ipv6 and ipv4 together
		{
			"192.167.0.1 2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			"192.167.x.x 2001:0db8:x",
		},
		{
			"node [5Btdd] d:e89a3384f526d251 now using 10.0.0.222:41641 mtu=1360 tx=d81a8a35a0ce 2345:0425:2CA1::0567:5673:23b5",
			"node [5Btdd] d:e89a3384f526d251 now using 10.0.x.x:41641 mtu=1360 tx=d81a8a35a0ce 2345:0425:x",
		},
		{
			"100.64.5.6 2091:0db8:85a3:0000:0000:8a2e:0370:7334",
			"100.64.5.6 2091:0db8:x",
		},
		{
			"192.167.0.1 120.100.30.47 2041:0000:140F::875B:131B",
			"192.167.x.x 120.100.x.x 2041:0000:x",
		},
		{
			"fd7a:115c:a1e0::/96 192.167.0.1 2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			"fd7a:115c:a1e0::/96 192.167.x.x 2001:0db8:x",
		},
	}

	for _, tt := range tests {
		gotBuf := redactIPs([]byte(tt.in))
		if string(gotBuf) != tt.want {
			t.Errorf("for %q,\n got: %#q\nwant: %#q\n", tt.in, gotBuf, tt.want)
		}
	}
}
