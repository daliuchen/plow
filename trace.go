package main

import "time"

// Latency phases of a single request, in the order they happen on the wire.
//
// Connect and TLS only happen when the request had to establish a new
// connection, so their sample count is the number of connections opened, not
// the number of requests. The remaining phases are recorded for every
// successful request.
const (
	phaseConnect  = iota // dial: DNS resolution + TCP handshake (or proxy/unix connect)
	phaseTLS             // TLS handshake
	phaseWrite           // writing the request to the socket
	phaseWait            // time to first response byte, i.e. the server thinking
	phaseRead            // reading the rest of the response off the socket
	phaseOverhead        // whatever is left: connection pool, header parsing, scheduling
	phaseCount
)

var phaseNames = [phaseCount]string{"Connect", "TLS", "Write", "Wait", "Read", "Overhead"}

// phaseTrace records the timeline of one request as observed at the connection
// level.
//
// It is owned by exactly one worker goroutine: the worker resets it before each
// request, the connection that worker dialed writes into it while the request
// is in flight, and the worker reads it back once the request is done. Every
// access therefore happens on the same goroutine and no locking is needed. This
// is also why enabling the breakdown gives each worker its own HostClient - a
// shared connection pool would let one worker's connection write into another
// worker's trace.
type phaseTrace struct {
	dialed   bool
	dialCost time.Duration
	tlsCost  time.Duration

	firstWrite time.Time
	lastWrite  time.Time
	firstRead  time.Time
	lastRead   time.Time
}

// reset clears the trace before a new request. Connection level costs are
// cleared too: they are only reported for the request that opened the
// connection.
func (t *phaseTrace) reset() {
	t.dialed = false
	t.dialCost = 0
	t.tlsCost = 0
	t.resetIO()
}

func (t *phaseTrace) resetIO() {
	t.firstWrite = time.Time{}
	t.lastWrite = time.Time{}
	t.firstRead = time.Time{}
	t.lastRead = time.Time{}
}

func (t *phaseTrace) onDial(cost time.Duration) {
	t.dialed = true
	t.dialCost = cost
}

func (t *phaseTrace) onHandshake(cost time.Duration) {
	t.tlsCost = cost
	// The handshake exchanges bytes on the connection before the request is
	// written. Drop those timestamps so they are not mistaken for the request.
	t.resetIO()
}

// onWrite takes both ends of the write because a request usually leaves in a
// single call: timing it from the start of the first write to the end of the
// last one is what keeps the time spent in the syscall inside the write phase
// instead of silently landing in the overhead.
func (t *phaseTrace) onWrite(start, end time.Time) {
	if t.firstWrite.IsZero() {
		t.firstWrite = start
	}
	t.lastWrite = end
}

// onRead takes the moment a read returned, i.e. when those bytes had arrived.
// The blocking part of the first read is the server thinking, and belongs to
// the wait phase rather than to the read phase.
func (t *phaseTrace) onRead(now time.Time) {
	if t.firstRead.IsZero() {
		t.firstRead = now
	}
	t.lastRead = now
}

// fill copies the collected timeline into a report record. rr.cost must already
// be set: the overhead phase is whatever the measured latency has left after
// all the observed phases are subtracted.
func (t *phaseTrace) fill(rr *ReportRecord) {
	rr.hasPhases = true
	rr.dialed = t.dialed
	rr.phases[phaseConnect] = t.dialCost
	rr.phases[phaseTLS] = t.tlsCost
	rr.phases[phaseWrite] = since(t.firstWrite, t.lastWrite)
	rr.phases[phaseWait] = since(t.lastWrite, t.firstRead)
	rr.phases[phaseRead] = since(t.firstRead, t.lastRead)

	var sum time.Duration
	for i := 0; i < phaseOverhead; i++ {
		sum += rr.phases[i]
	}
	if rr.cost > sum {
		rr.phases[phaseOverhead] = rr.cost - sum
	} else {
		rr.phases[phaseOverhead] = 0
	}
}

// since returns end-start, or 0 when either end is missing or the ordering is
// unexpected, so a partially observed request can never contribute a negative
// sample.
func since(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 0
	}
	return end.Sub(start)
}
