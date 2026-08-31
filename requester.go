package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	url2 "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
	"go.uber.org/automaxprocs/maxprocs"
	"golang.org/x/time/rate"
)

var (
	startTimeUnixNano int64
	sendOnCloseError  interface{}
)

type ReportRecord struct {
	cost             time.Duration
	code             int
	error            string
	readBytes        int64
	writeBytes       int64
	concurrencyCount int

	// Latency breakdown, only filled when --breakdown is enabled and the
	// request succeeded. dialed reports whether this request had to open a new
	// connection, which is what makes the Connect and TLS phases meaningful.
	hasPhases bool
	dialed    bool
	phases    [phaseCount]time.Duration
}

var recordPool = sync.Pool{
	New: func() interface{} { return new(ReportRecord) },
}

func init() {
	// Honoring env GOMAXPROCS
	_, _ = maxprocs.Set()
	defer func() {
		sendOnCloseError = recover()
	}()
	func() {
		cc := make(chan struct{}, 1)
		close(cc)
		cc <- struct{}{}
	}()
}

type MyConn struct {
	net.Conn
	r, w *int64
	// trace is nil unless the latency breakdown is enabled. It belongs to the
	// single worker goroutine that dialed this connection.
	trace *phaseTrace
}

func NewMyConn(conn net.Conn, r, w *int64, trace *phaseTrace) (*MyConn, error) {
	myConn := &MyConn{Conn: conn, r: r, w: w, trace: trace}
	return myConn, nil
}

func (c *MyConn) Read(b []byte) (n int, err error) {
	sz, err := c.Conn.Read(b)

	if err == nil {
		atomic.AddInt64(c.r, int64(sz))
		if c.trace != nil {
			c.trace.onRead(time.Now())
		}
	}
	return sz, err
}

func (c *MyConn) Write(b []byte) (n int, err error) {
	var start time.Time
	if c.trace != nil {
		start = time.Now()
	}
	sz, err := c.Conn.Write(b)

	if err == nil {
		atomic.AddInt64(c.w, int64(sz))
		if c.trace != nil {
			c.trace.onWrite(start, time.Now())
		}
	}
	return sz, err
}

func ThroughputInterceptorDial(dial fasthttp.DialFunc, r *int64, w *int64, trace *phaseTrace) fasthttp.DialFunc {
	return func(addr string) (net.Conn, error) {
		start := time.Now()
		conn, err := dial(addr)
		if err != nil {
			return nil, err
		}
		if trace != nil {
			trace.onDial(time.Since(start))
		}
		return NewMyConn(conn, r, w, trace)
	}
}

// TLSInterceptorDial performs the TLS handshake itself so it can be timed
// separately from the dial and from the request.
//
// fasthttp normally handshakes right after calling the dial func, which would
// leave the handshake bytes indistinguishable from the request bytes on the
// traced connection. It skips its own handshake when the dial func returns a
// connection that already exposes Handshake() error (see dialAddr in
// fasthttp/client.go), which is exactly what *tls.Conn does, so returning one
// here hands us the timing without changing what goes over the wire.
func TLSInterceptorDial(dial fasthttp.DialFunc, tlsConfig *tls.Config, timeout time.Duration, trace *phaseTrace) fasthttp.DialFunc {
	return func(addr string) (net.Conn, error) {
		conn, err := dial(addr)
		if err != nil {
			return nil, err
		}
		start := time.Now()
		tlsConn := tls.Client(conn, tlsConfig)
		if timeout > 0 {
			if err := tlsConn.SetDeadline(start.Add(timeout)); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if timeout > 0 {
			if err := tlsConn.SetDeadline(time.Time{}); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		trace.onHandshake(time.Since(start))
		return tlsConn, nil
	}
}

type Requester struct {
	concurrency int
	reqRate     *rate.Limit
	requests    int64
	duration    time.Duration
	rampUp      int
	clientOpt   *ClientOpt
	// httpClients has one entry per worker. Without the latency breakdown every
	// entry is the same shared client, so connection pooling is unchanged; with
	// it each worker gets its own single-connection client so that connection
	// level timings can be attributed to the request that caused them.
	httpClients []*fasthttp.HostClient
	traces      []*phaseTrace
	isTLS       bool
	httpHeader  *fasthttp.RequestHeader
	errWriter   io.Writer

	recordChan chan *ReportRecord
	closeOnce  sync.Once
	wg         sync.WaitGroup

	readBytes  int64
	writeBytes int64

	cancel func()
}

type ClientOpt struct {
	url       string
	method    string
	headers   []string
	bodyBytes []byte
	bodyFile  string

	certPath string
	keyPath  string
	insecure bool

	maxConns     int
	doTimeout    time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration
	dialTimeout  time.Duration

	socks5Proxy string
	httpProxy   string
	contentType string
	host        string
	unixSocket  string

	breakdown bool
}

func NewRequester(concurrency int, requests int64, duration time.Duration, reqRate *rate.Limit, errWriter io.Writer, clientOpt *ClientOpt, rampUp int) (*Requester, error) {
	maxResult := concurrency * 100
	if maxResult > 8192 {
		maxResult = 8192
	}
	r := &Requester{
		concurrency: concurrency,
		reqRate:     reqRate,
		requests:    requests,
		duration:    duration,
		rampUp:      rampUp,
		errWriter:   errWriter,
		clientOpt:   clientOpt,
		recordChan:  make(chan *ReportRecord, maxResult),
		httpClients: make([]*fasthttp.HostClient, concurrency),
	}
	if clientOpt.breakdown {
		r.traces = make([]*phaseTrace, concurrency)
		for i := 0; i < concurrency; i++ {
			// Own client, own connection, own trace: see phaseTrace.
			workerOpt := *clientOpt
			workerOpt.maxConns = 1
			trace := &phaseTrace{}
			client, header, err := buildTracedRequestClient(&workerOpt, &r.readBytes, &r.writeBytes, trace)
			if err != nil {
				return nil, err
			}
			r.httpClients[i] = client
			r.traces[i] = trace
			r.httpHeader = header
		}
	} else {
		client, header, err := buildRequestClient(clientOpt, &r.readBytes, &r.writeBytes)
		if err != nil {
			return nil, err
		}
		for i := range r.httpClients {
			r.httpClients[i] = client
		}
		r.httpHeader = header
	}
	if concurrency > 0 {
		r.isTLS = r.httpClients[0].IsTLS
	}
	return r, nil
}

func (r *Requester) trace(idx int) *phaseTrace {
	if r.traces == nil {
		return nil
	}
	return r.traces[idx]
}

func addMissingPort(addr string, isTLS bool) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	port := 80
	if isTLS {
		port = 443
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(port))
}

func buildTLSConfig(opt *ClientOpt) (*tls.Config, error) {
	var certs []tls.Certificate
	if opt.certPath != "" && opt.keyPath != "" {
		c, err := tls.LoadX509KeyPair(opt.certPath, opt.keyPath)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	return &tls.Config{
		InsecureSkipVerify: opt.insecure,
		Certificates:       certs,
	}, nil
}

// tlsServerName mirrors how fasthttp derives the SNI name from the address it
// dials, so taking over the handshake does not change certificate validation.
// A "*" means the address is not a usable server name.
func tlsServerName(addr string) string {
	if !strings.Contains(addr, ":") {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "*"
	}
	return host
}

func buildRequestClient(opt *ClientOpt, r *int64, w *int64) (*fasthttp.HostClient, *fasthttp.RequestHeader, error) {
	return buildTracedRequestClient(opt, r, w, nil)
}

func buildTracedRequestClient(opt *ClientOpt, r *int64, w *int64, trace *phaseTrace) (*fasthttp.HostClient, *fasthttp.RequestHeader, error) {
	u, err := url2.Parse(opt.url)
	if err != nil {
		return nil, nil, err
	}
	httpClient := &fasthttp.HostClient{
		Addr:                          addMissingPort(u.Host, u.Scheme == "https"),
		IsTLS:                         u.Scheme == "https",
		Name:                          "plow",
		MaxConns:                      opt.maxConns,
		ReadTimeout:                   opt.readTimeout,
		WriteTimeout:                  opt.writeTimeout,
		DisableHeaderNamesNormalizing: true,
	}
	if opt.socks5Proxy != "" {
		if !strings.Contains(opt.socks5Proxy, "://") {
			opt.socks5Proxy = "socks5://" + opt.socks5Proxy
		}
		httpClient.Dial = fasthttpproxy.FasthttpSocksDialer(opt.socks5Proxy)
	} else if opt.unixSocket != "" {
		httpClient.Dial = func(addr string) (net.Conn, error) {
			return net.Dial("unix", opt.unixSocket)
		}
	} else if opt.httpProxy != "" {
		httpClient.Dial = fasthttpproxy.FasthttpHTTPDialerDualStack(opt.httpProxy)
	} else {
		dialer := fasthttpproxy.Dialer{
			Timeout:        opt.dialTimeout,
			ConnectTimeout: opt.dialTimeout,
			DialDualStack:  true,
		}
		httpClient.Dial, err = dialer.GetDialFunc(true)
		if err != nil {
			return nil, nil, err
		}
	}
	httpClient.Dial = ThroughputInterceptorDial(httpClient.Dial, r, w, trace)

	tlsConfig, err := buildTLSConfig(opt)
	if err != nil {
		return nil, nil, err
	}
	httpClient.TLSConfig = tlsConfig

	if trace != nil && httpClient.IsTLS {
		handshakeConfig := tlsConfig.Clone()
		if handshakeConfig.ServerName == "" {
			if serverName := tlsServerName(httpClient.Addr); serverName == "*" {
				handshakeConfig.InsecureSkipVerify = true
			} else {
				handshakeConfig.ServerName = serverName
			}
		}
		httpClient.Dial = TLSInterceptorDial(httpClient.Dial, handshakeConfig, opt.dialTimeout, trace)
	}

	var requestHeader fasthttp.RequestHeader
	if opt.contentType != "" {
		requestHeader.SetContentType(opt.contentType)
	}
	if opt.host != "" {
		requestHeader.SetHost(opt.host)
	} else {
		requestHeader.SetHost(u.Host)
	}
	requestHeader.SetMethod(opt.method)
	requestHeader.SetRequestURI(u.RequestURI())
	for _, h := range opt.headers {
		n := strings.SplitN(h, ":", 2)
		if len(n) != 2 {
			return nil, nil, fmt.Errorf("invalid header: %s", h)
		}
		requestHeader.Set(strings.TrimSpace(n[0]), strings.TrimSpace(n[1]))
	}

	return httpClient, &requestHeader, nil
}

func (r *Requester) Cancel() {
	r.cancel()
}

func (r *Requester) RecordChan() <-chan *ReportRecord {
	return r.recordChan
}

func (r *Requester) closeRecord() {
	r.closeOnce.Do(func() {
		close(r.recordChan)
	})
}

func (r *Requester) DoRequest(client *fasthttp.HostClient, trace *phaseTrace, req *fasthttp.Request, resp *fasthttp.Response, rr *ReportRecord) {
	startTime := time.Unix(0, atomic.LoadInt64(&startTimeUnixNano))
	t1 := time.Since(startTime)
	rr.hasPhases = false
	rr.dialed = false
	if trace != nil {
		trace.reset()
	}
	var err error
	if r.clientOpt.doTimeout > 0 {
		err = client.DoTimeout(req, resp, r.clientOpt.doTimeout)
	} else {
		err = client.Do(req, resp)
	}

	if err != nil {
		rr.cost = time.Since(startTime) - t1
		rr.error = err.Error()
		return
	}

	writeTo := io.Discard
	if resp.StatusCode() >= 500 {
		writeTo = r.errWriter
		_, _ = r.errWriter.Write([]byte(fmt.Sprintf("\n%d %s\n", resp.StatusCode(), rr.cost)))
		_, _ = r.errWriter.Write([]byte(fmt.Sprintf("%s", &resp.Header)))
	}
	err = resp.BodyWriteTo(writeTo)
	if err != nil {
		rr.cost = time.Since(startTime) - t1
		rr.error = err.Error()
		return
	}

	rr.cost = time.Since(startTime) - t1
	rr.code = resp.StatusCode()
	rr.error = ""
	if trace != nil {
		trace.fill(rr)
	}
}

func (r *Requester) Run() {
	// handle ctrl-c
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	ctx, cancelFunc := context.WithCancel(context.Background())
	r.cancel = cancelFunc
	signalDone := make(chan struct{})
	go func() {
		defer close(signalDone)
		select {
		case <-sigs:
			r.closeRecord()
			cancelFunc()
		case <-ctx.Done():
		}
	}()
	defer func() {
		signal.Stop(sigs)
		cancelFunc()
		<-signalDone
	}()
	atomic.StoreInt64(&startTimeUnixNano, time.Now().UnixNano())
	if r.duration > 0 {
		time.AfterFunc(r.duration, func() {
			r.closeRecord()
			cancelFunc()
		})
	}

	var limiter *rate.Limiter
	if r.reqRate != nil {
		limiter = rate.NewLimiter(*r.reqRate, 1)
	}

	semaphore := r.requests
	if r.rampUp <= 0 {
		r.rampUp = r.concurrency
	}
	concurrencyCount := 0
	loopCount := int(math.Ceil(float64(r.concurrency) / float64(r.rampUp)))
	for i := 0; i < loopCount; i++ {
		for j := 0; j < r.rampUp; j++ {
			if concurrencyCount >= r.concurrency {
				break
			}
			concurrencyCount++
			workerIdx := concurrencyCount - 1
			r.wg.Add(1)
			go func() {
				defer func() {
					r.wg.Done()
					v := recover()
					if v != nil && v != sendOnCloseError {
						panic(v)
					}
				}()
				client := r.httpClients[workerIdx]
				trace := r.trace(workerIdx)
				req := &fasthttp.Request{}
				resp := &fasthttp.Response{}
				r.httpHeader.CopyTo(&req.Header)
				if r.isTLS {
					req.URI().SetScheme("https")
					req.URI().SetHostBytes(req.Header.Host())
				}

				for {
					select {
					case <-ctx.Done():
						return
					default:
					}

					if limiter != nil {
						err := limiter.Wait(ctx)
						if err != nil {
							continue
						}
					}

					if r.requests > 0 && atomic.AddInt64(&semaphore, -1) < 0 {
						cancelFunc()
						return
					}

					if r.clientOpt.bodyFile != "" {
						file, err := os.Open(r.clientOpt.bodyFile)
						if err != nil {
							rr := recordPool.Get().(*ReportRecord)
							rr.cost = 0
							rr.error = err.Error()
							rr.hasPhases = false
							rr.readBytes = atomic.LoadInt64(&r.readBytes)
							rr.writeBytes = atomic.LoadInt64(&r.writeBytes)
							rr.concurrencyCount = concurrencyCount
							r.recordChan <- rr
							continue
						}
						req.SetBodyStream(file, -1)
					} else {
						req.SetBodyRaw(r.clientOpt.bodyBytes)
					}
					resp.Reset()
					rr := recordPool.Get().(*ReportRecord)
					r.DoRequest(client, trace, req, resp, rr)
					rr.readBytes = atomic.LoadInt64(&r.readBytes)
					rr.writeBytes = atomic.LoadInt64(&r.writeBytes)
					rr.concurrencyCount = concurrencyCount
					r.recordChan <- rr
				}
			}()
		}
		if r.rampUp != r.concurrency {
			time.Sleep(time.Second)
		}
	}

	r.wg.Wait()
	r.closeRecord()
}
