package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPhaseTraceFill(t *testing.T) {
	base := time.Now()
	trace := &phaseTrace{}
	trace.onDial(3 * time.Millisecond)
	trace.onHandshake(5 * time.Millisecond)
	trace.onWrite(base, base.Add(time.Millisecond))
	trace.onRead(base.Add(21 * time.Millisecond))
	trace.onRead(base.Add(23 * time.Millisecond))

	rr := &ReportRecord{cost: 40 * time.Millisecond}
	trace.fill(rr)

	if !rr.hasPhases || !rr.dialed {
		t.Fatalf("hasPhases/dialed = %v/%v, want both true", rr.hasPhases, rr.dialed)
	}
	want := map[int]time.Duration{
		phaseConnect:  3 * time.Millisecond,
		phaseTLS:      5 * time.Millisecond,
		phaseWrite:    time.Millisecond,
		phaseWait:     20 * time.Millisecond,
		phaseRead:     2 * time.Millisecond,
		phaseOverhead: 9 * time.Millisecond,
	}
	for phase, d := range want {
		if rr.phases[phase] != d {
			t.Fatalf("%s = %s, want %s", phaseNames[phase], rr.phases[phase], d)
		}
	}
}

func TestPhaseTraceFillClampsOverheadAndMissingTimestamps(t *testing.T) {
	base := time.Now()
	trace := &phaseTrace{}
	trace.onWrite(base, base.Add(10*time.Millisecond))
	// No read ever happened, and the measured cost is smaller than the phases
	// observed inside it, which a coarse clock can produce on a fast request.
	rr := &ReportRecord{cost: time.Millisecond}
	trace.fill(rr)

	if rr.dialed {
		t.Fatal("dialed = true, want false for a reused connection")
	}
	if rr.phases[phaseWait] != 0 || rr.phases[phaseRead] != 0 {
		t.Fatalf("wait/read = %s/%s, want both zero", rr.phases[phaseWait], rr.phases[phaseRead])
	}
	if rr.phases[phaseOverhead] != 0 {
		t.Fatalf("overhead = %s, want zero rather than negative", rr.phases[phaseOverhead])
	}
}

func TestPhaseTraceResetDropsHandshakeIO(t *testing.T) {
	base := time.Now()
	trace := &phaseTrace{}
	// A TLS handshake writes and reads on the connection before the request is
	// written. Those bytes must not be mistaken for the request itself.
	trace.onWrite(base, base.Add(time.Millisecond))
	trace.onRead(base.Add(2 * time.Millisecond))
	trace.onHandshake(4 * time.Millisecond)

	if !trace.firstWrite.IsZero() || !trace.firstRead.IsZero() {
		t.Fatalf("handshake I/O timestamps survived onHandshake: %+v", trace)
	}

	trace.onWrite(base.Add(5*time.Millisecond), base.Add(6*time.Millisecond))
	trace.onRead(base.Add(9 * time.Millisecond))
	rr := &ReportRecord{cost: 20 * time.Millisecond}
	trace.fill(rr)

	if rr.phases[phaseWait] != 3*time.Millisecond {
		t.Fatalf("wait = %s, want 3ms measured from the request write", rr.phases[phaseWait])
	}
	if rr.phases[phaseTLS] != 4*time.Millisecond {
		t.Fatalf("tls = %s, want 4ms", rr.phases[phaseTLS])
	}
}

func TestSince(t *testing.T) {
	base := time.Now()
	if got := since(base, base.Add(time.Second)); got != time.Second {
		t.Fatalf("since() = %s, want 1s", got)
	}
	if got := since(time.Time{}, base); got != 0 {
		t.Fatalf("since() with no start = %s, want 0", got)
	}
	if got := since(base, time.Time{}); got != 0 {
		t.Fatalf("since() with no end = %s, want 0", got)
	}
	if got := since(base.Add(time.Second), base); got != 0 {
		t.Fatalf("since() going backwards = %s, want 0", got)
	}
}

func TestTLSServerName(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{addr: "example.com", want: "example.com"},
		{addr: "example.com:443", want: "example.com"},
		{addr: "[::1]:8443", want: "::1"},
		{addr: "a:b:c", want: "*"},
	}
	for _, tt := range tests {
		if got := tlsServerName(tt.addr); got != tt.want {
			t.Fatalf("tlsServerName(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

// TestRequesterBreakdownOverTLS is the end to end check of the breakdown: the
// connection phases land on the request that opened the connection, the request
// phases stay clean on every request, and - most importantly - the TLS
// handshake is timed as TLS instead of leaking into the request timings. The
// last part depends on fasthttp skipping its own handshake when the dial func
// returns an already handshaken connection, so this test also guards that
// assumption across fasthttp upgrades.
func TestRequesterBreakdownOverTLS(t *testing.T) {
	const serverThinkTime = 30 * time.Millisecond
	body := bytes.Repeat([]byte("plow"), 128*1024)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(serverThinkTime)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	requester, err := NewRequester(1, 3, 0, nil, io.Discard, &ClientOpt{
		url:         srv.URL + "/",
		method:      http.MethodGet,
		maxConns:    1,
		insecure:    true,
		breakdown:   true,
		dialTimeout: 5 * time.Second,
		doTimeout:   5 * time.Second,
	}, -1)
	if err != nil {
		t.Fatal(err)
	}

	go requester.Run()
	var records []*ReportRecord
	for record := range requester.RecordChan() {
		copied := *record
		records = append(records, &copied)
	}
	if len(records) != 3 {
		t.Fatalf("records len = %d, want 3", len(records))
	}

	for i, record := range records {
		if record.error != "" || record.code != http.StatusOK {
			t.Fatalf("record[%d] = code %d error %q, want 200 with no error", i, record.code, record.error)
		}
		if !record.hasPhases {
			t.Fatalf("record[%d] has no latency breakdown", i)
		}
		if record.phases[phaseWait] < serverThinkTime {
			t.Fatalf("record[%d] wait = %s, want at least the server think time %s (a handshake or the response body leaking into the wait phase would shrink it)",
				i, record.phases[phaseWait], serverThinkTime)
		}
		if record.phases[phaseWrite] <= 0 {
			t.Fatalf("record[%d] write = %s, want positive", i, record.phases[phaseWrite])
		}
		if record.phases[phaseRead] <= 0 {
			t.Fatalf("record[%d] read = %s, want positive for a %d byte body", i, record.phases[phaseRead], len(body))
		}
		var sum time.Duration
		for _, d := range record.phases {
			sum += d
		}
		if sum != record.cost {
			t.Fatalf("record[%d] phases add up to %s, want the measured cost %s", i, sum, record.cost)
		}
	}

	first := records[0]
	if !first.dialed {
		t.Fatal("first record did not open a connection")
	}
	if first.phases[phaseConnect] <= 0 || first.phases[phaseTLS] <= 0 {
		t.Fatalf("first record connect/tls = %s/%s, want both positive",
			first.phases[phaseConnect], first.phases[phaseTLS])
	}
	for i, record := range records[1:] {
		if record.dialed {
			t.Fatalf("record[%d] opened a new connection, want keep-alive reuse", i+1)
		}
		if record.phases[phaseConnect] != 0 || record.phases[phaseTLS] != 0 {
			t.Fatalf("record[%d] connect/tls = %s/%s, want zero on a reused connection",
				i+1, record.phases[phaseConnect], record.phases[phaseTLS])
		}
	}
}

func TestRequesterWithoutBreakdownRecordsNoPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	requester, err := NewRequester(2, 4, 0, nil, io.Discard, &ClientOpt{
		url:         srv.URL + "/",
		method:      http.MethodGet,
		maxConns:    2,
		dialTimeout: 5 * time.Second,
		doTimeout:   5 * time.Second,
	}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if requester.traces != nil {
		t.Fatal("traces were allocated without --breakdown")
	}
	if requester.httpClients[0] != requester.httpClients[1] {
		t.Fatal("workers got separate clients without --breakdown, want the shared connection pool")
	}

	go requester.Run()
	for record := range requester.RecordChan() {
		if record.hasPhases {
			t.Fatal("record carries a latency breakdown without --breakdown")
		}
	}
}
