package main

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beorn7/perks/histogram"
	"github.com/beorn7/perks/quantile"
)

var quantiles = []float64{0.50, 0.75, 0.90, 0.95, 0.99, 0.999, 0.9999}

var quantilesTarget = map[float64]float64{
	0.50:   0.01,
	0.75:   0.01,
	0.90:   0.001,
	0.95:   0.001,
	0.99:   0.001,
	0.999:  0.0001,
	0.9999: 0.00001,
}

// Phase latencies are tracked with a coarser target set than the overall
// latency: they exist to explain where the time goes, not to be reported at
// four nines, and a tighter stream costs more per inserted sample.
var phaseQuantiles = []float64{0.50, 0.90, 0.99}

var phaseQuantilesTarget = map[float64]float64{
	0.50: 0.01,
	0.90: 0.001,
	0.99: 0.001,
}

var httpStatusSectionLabelMap = map[int]string{
	1: "1xx",
	2: "2xx",
	3: "3xx",
	4: "4xx",
	5: "5xx",
}

type Stats struct {
	count int64
	sum   float64
	sumSq float64
	min   float64
	max   float64
}

func (s *Stats) Update(v float64) {
	s.count++
	s.sum += v
	s.sumSq += v * v
	if v < s.min || s.count == 1 {
		s.min = v
	}
	if v > s.max || s.count == 1 {
		s.max = v
	}
}

func (s *Stats) Stddev() float64 {
	num := (float64(s.count) * s.sumSq) - math.Pow(s.sum, 2)
	div := float64(s.count * (s.count - 1))
	if div == 0 {
		return 0
	}
	return math.Sqrt(num / div)
}

func (s *Stats) Mean() float64 {
	if s.count == 0 {
		return 0
	}
	return s.sum / float64(s.count)
}

func (s *Stats) Reset() {
	s.count = 0
	s.sum = 0
	s.sumSq = 0
	s.min = 0
	s.max = 0
}

// phaseAgg accumulates the samples of a single latency phase.
type phaseAgg struct {
	stats    Stats
	quantile *quantile.Stream
}

func (p *phaseAgg) insert(v float64) {
	p.stats.Update(v)
	p.quantile.Insert(v)
}

type StreamReport struct {
	lock sync.Mutex

	// breakdown is nil unless --breakdown is enabled.
	breakdown        []*phaseAgg
	phaseWithinSec   [phaseCount]Stats
	latencyStats     *Stats
	rpsStats         *Stats
	latencyQuantile  *quantile.Stream
	latencyHistogram *histogram.Histogram
	codes            map[int]int64
	errors           map[string]int64
	concurrencyCount int

	latencyWithinSec *Stats
	rpsWithinSec     float64
	noDateWithinSec  bool

	readBytes  int64
	writeBytes int64

	doneChan chan struct{}
}

func NewStreamReport(breakdown bool) *StreamReport {
	var phases []*phaseAgg
	if breakdown {
		phases = make([]*phaseAgg, phaseCount)
		for i := range phases {
			phases[i] = &phaseAgg{quantile: quantile.NewTargeted(phaseQuantilesTarget)}
		}
	}
	return &StreamReport{
		breakdown:        phases,
		latencyQuantile:  quantile.NewTargeted(quantilesTarget),
		latencyHistogram: histogram.New(8),
		codes:            make(map[int]int64, 1),
		errors:           make(map[string]int64, 1),
		doneChan:         make(chan struct{}, 1),
		latencyStats:     &Stats{},
		rpsStats:         &Stats{},
		latencyWithinSec: &Stats{},
	}
}

func (s *StreamReport) insert(v float64) {
	s.latencyQuantile.Insert(v)
	s.latencyHistogram.Insert(v)
	s.latencyStats.Update(v)
}

// insertPhases records the latency breakdown of one request. Connection level
// phases are only sampled for the request that actually opened the connection,
// otherwise every keep-alive request would dilute them with zeros.
func (s *StreamReport) insertPhases(r *ReportRecord, withinSec *[phaseCount]Stats) {
	for i, d := range r.phases {
		if (i == phaseConnect && !r.dialed) || (i == phaseTLS && (!r.dialed || d == 0)) {
			continue
		}
		s.breakdown[i].insert(float64(d))
		withinSec[i].Update(float64(d))
	}
}

func (s *StreamReport) Collect(records <-chan *ReportRecord) {
	latencyWithinSecTemp := &Stats{}
	var phaseWithinSecTemp [phaseCount]Stats
	go func() {
		startTime := time.Unix(0, atomic.LoadInt64(&startTimeUnixNano))
		ticker := time.NewTicker(time.Second)
		lastCount := int64(0)
		lastTime := startTime
		for {
			select {
			case <-ticker.C:
				s.lock.Lock()
				dc := s.latencyStats.count - lastCount
				if dc > 0 {
					rps := float64(dc) / time.Since(lastTime).Seconds()
					s.rpsStats.Update(rps)
					lastCount = s.latencyStats.count
					lastTime = time.Now()

					*s.latencyWithinSec = *latencyWithinSecTemp
					s.rpsWithinSec = rps
					latencyWithinSecTemp.Reset()
					s.phaseWithinSec = phaseWithinSecTemp
					for i := range phaseWithinSecTemp {
						phaseWithinSecTemp[i].Reset()
					}
					s.noDateWithinSec = false
				} else {
					s.noDateWithinSec = true
				}
				s.lock.Unlock()
			case <-s.doneChan:
				return
			}
		}
	}()

	for {
		r, ok := <-records
		if !ok {
			close(s.doneChan)
			break
		}
		s.lock.Lock()
		latencyWithinSecTemp.Update(float64(r.cost))
		s.insert(float64(r.cost))
		if r.hasPhases && s.breakdown != nil {
			s.insertPhases(r, &phaseWithinSecTemp)
		}
		if r.code != 0 {
			s.codes[r.code]++
		}
		if r.error != "" {
			s.errors[r.error]++
		}
		s.readBytes = r.readBytes
		s.writeBytes = r.writeBytes
		s.concurrencyCount = r.concurrencyCount
		s.lock.Unlock()
		recordPool.Put(r)
	}
}
func (s *StreamReport) copyCodes() map[int]int64 {
	res := make(map[int]int64, len(s.codes))
	for k, v := range s.codes {
		res[k] = v
	}
	return res
}

type SnapshotReport struct {
	Elapsed          time.Duration
	Count            int64
	Codes            map[string]int64
	Errors           map[string]int64
	RPS              float64
	ReadThroughput   float64
	WriteThroughput  float64
	concurrencyCount int

	Stats *struct {
		Min    time.Duration
		Mean   time.Duration
		StdDev time.Duration
		Max    time.Duration
	}

	RpsStats *struct {
		Min    float64
		Mean   float64
		StdDev float64
		Max    float64
	}

	Percentiles []*struct {
		Percentile float64
		Latency    time.Duration
	}

	Histograms []*struct {
		Mean  time.Duration
		Count int
	}

	// Breakdown is nil unless --breakdown is enabled.
	Breakdown []*PhaseReport
}

// PhaseReport is the latency of one request phase. Count is the number of
// samples, which for the connection level phases is the number of connections
// opened rather than the number of requests.
type PhaseReport struct {
	Name        string
	Count       int64
	Mean        time.Duration
	Max         time.Duration
	Percentiles []time.Duration
}

func (s *StreamReport) Snapshot() *SnapshotReport {
	s.lock.Lock()
	startTime := time.Unix(0, atomic.LoadInt64(&startTimeUnixNano))
	rs := &SnapshotReport{
		Elapsed: time.Since(startTime),
		Count:   s.latencyStats.count,
		Stats: &struct {
			Min    time.Duration
			Mean   time.Duration
			StdDev time.Duration
			Max    time.Duration
		}{time.Duration(s.latencyStats.min), time.Duration(s.latencyStats.Mean()),
			time.Duration(s.latencyStats.Stddev()), time.Duration(s.latencyStats.max)},
	}
	if s.rpsStats.count > 0 {
		rs.RpsStats = &struct {
			Min    float64
			Mean   float64
			StdDev float64
			Max    float64
		}{s.rpsStats.min, s.rpsStats.Mean(),
			s.rpsStats.Stddev(), s.rpsStats.max}
	}

	elapseInSec := rs.Elapsed.Seconds()
	rs.RPS = float64(rs.Count) / elapseInSec
	rs.ReadThroughput = float64(s.readBytes) / 1024.0 / 1024.0 / elapseInSec
	rs.WriteThroughput = float64(s.writeBytes) / 1024.0 / 1024.0 / elapseInSec
	rs.concurrencyCount = s.concurrencyCount

	rs.Codes = make(map[string]int64, len(s.codes))
	for k, v := range s.codes {
		section := k / 100
		rs.Codes[httpStatusSectionLabelMap[section]] += v
	}
	rs.Errors = make(map[string]int64, len(s.errors))
	for k, v := range s.errors {
		rs.Errors[k] = v
	}

	rs.Percentiles = make([]*struct {
		Percentile float64
		Latency    time.Duration
	}, len(quantiles))
	for i, p := range quantiles {
		rs.Percentiles[i] = &struct {
			Percentile float64
			Latency    time.Duration
		}{p, time.Duration(s.latencyQuantile.Query(p))}
	}

	for i, phase := range s.breakdown {
		if phase.stats.count == 0 {
			continue
		}
		pr := &PhaseReport{
			Name:        phaseNames[i],
			Count:       phase.stats.count,
			Mean:        time.Duration(phase.stats.Mean()),
			Max:         time.Duration(phase.stats.max),
			Percentiles: make([]time.Duration, len(phaseQuantiles)),
		}
		for j, q := range phaseQuantiles {
			pr.Percentiles[j] = time.Duration(phase.quantile.Query(q))
		}
		rs.Breakdown = append(rs.Breakdown, pr)
	}

	hisBins := s.latencyHistogram.Bins()
	rs.Histograms = make([]*struct {
		Mean  time.Duration
		Count int
	}, len(hisBins))
	for i, b := range hisBins {
		rs.Histograms[i] = &struct {
			Mean  time.Duration
			Count int
		}{time.Duration(b.Mean()), b.Count}
	}

	s.lock.Unlock()
	return rs
}

func (s *StreamReport) Done() <-chan struct{} {
	return s.doneChan
}

type ChartsReport struct {
	RPS         float64
	Latency     Stats
	CodeMap     map[int]int64
	Concurrency int
	// Breakdown holds the mean of each per-request phase over the last second,
	// nil unless --breakdown is enabled.
	Breakdown []float64
}

// chartPhases are the phases plotted over time. The connection level phases are
// left out on purpose: they only happen on a handful of requests, so a line
// through them says more about when a connection was opened than about the
// latency of the second it lands in.
var chartPhases = []int{phaseWrite, phaseWait, phaseRead, phaseOverhead}

func (s *StreamReport) Charts() *ChartsReport {
	s.lock.Lock()
	var cr *ChartsReport
	if s.noDateWithinSec {
		cr = nil
	} else {
		cr = &ChartsReport{
			RPS:         s.rpsWithinSec,
			Latency:     *s.latencyWithinSec,
			CodeMap:     s.copyCodes(),
			Concurrency: s.concurrencyCount,
		}
		if s.breakdown != nil {
			cr.Breakdown = make([]float64, len(chartPhases))
			for i, phase := range chartPhases {
				cr.Breakdown[i] = s.phaseWithinSec[phase].Mean()
			}
		}
	}
	s.lock.Unlock()
	return cr
}
