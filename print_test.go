package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testSnapshotReport() *SnapshotReport {
	return &SnapshotReport{
		Elapsed:          1500 * time.Millisecond,
		Count:            3,
		Codes:            map[string]int64{"2xx": 2, "5xx": 1},
		Errors:           map[string]int64{"connection reset by peer": 1},
		RPS:              2,
		ReadThroughput:   1.25,
		WriteThroughput:  0.5,
		concurrencyCount: 2,
		Stats: &struct {
			Min    time.Duration
			Mean   time.Duration
			StdDev time.Duration
			Max    time.Duration
		}{10 * time.Millisecond, 20 * time.Millisecond, 5 * time.Millisecond, 30 * time.Millisecond},
		RpsStats: &struct {
			Min    float64
			Mean   float64
			StdDev float64
			Max    float64
		}{1.11, 2.22, 0.33, 3.33},
		Percentiles: []*struct {
			Percentile float64
			Latency    time.Duration
		}{
			{0.50, 15 * time.Millisecond},
			{0.99, 29 * time.Millisecond},
		},
		Histograms: []*struct {
			Mean  time.Duration
			Count int
		}{
			{10 * time.Millisecond, 1},
			{30 * time.Millisecond, 2},
		},
	}
}

func TestPrinterFormatJSONReportsProducesValidJSON(t *testing.T) {
	printer := NewPrinter(3, 0, false, false)
	var buf bytes.Buffer
	printer.formatJSONReports(&buf, testSnapshotReport(), true, false)

	var got struct {
		Summary struct {
			Count  int64            `json:"Count"`
			Counts map[string]int64 `json:"Counts"`
		} `json:"Summary"`
		Error       map[string]int64  `json:"Error"`
		Statistics  json.RawMessage   `json:"Statistics"`
		Percentiles map[string]string `json:"Percentiles"`
		Histograms  json.RawMessage   `json:"Histograms"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("formatJSONReports produced invalid JSON: %v\n%s", err, buf.String())
	}

	for _, tt := range []struct {
		key string
		ok  bool
	}{
		{"Summary", got.Summary.Count != 0 || got.Summary.Counts != nil},
		{"Error", got.Error != nil},
		{"Statistics", len(got.Statistics) != 0},
		{"Percentiles", got.Percentiles != nil},
		{"Histograms", len(got.Histograms) != 0},
	} {
		if !tt.ok {
			t.Fatalf("JSON output is missing %q: %s", tt.key, buf.String())
		}
	}

	if got.Summary.Count != 3 {
		t.Fatalf("Summary.Count = %v, want 3", got.Summary.Count)
	}
	if got.Summary.Counts["2xx"] != 2 || got.Summary.Counts["5xx"] != 1 {
		t.Fatalf("Summary.Counts = %#v, want 2xx=2 and 5xx=1", got.Summary.Counts)
	}

	if got.Percentiles["P50"] != "15ms" || got.Percentiles["P99"] != "29ms" {
		t.Fatalf("Percentiles = %#v, want P50=15ms and P99=29ms", got.Percentiles)
	}
}

func TestPrinterFormatTableReportsContainsFriendlySections(t *testing.T) {
	printer := NewPrinter(3, time.Second, false, false)
	var buf bytes.Buffer
	printer.formatTableReports(&buf, testSnapshotReport(), true, false)
	out := buf.String()

	for _, want := range []string{
		"Summary:",
		"Elapsed",
		"Count",
		"2xx",
		"5xx",
		"Error:",
		"connection reset by peer",
		"Statistics",
		"Latency Percentile:",
		"Latency Histogram:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output is missing %q:\n%s", want, out)
		}
	}
}

func TestDurationToString(t *testing.T) {
	d := 1234567 * time.Microsecond
	if got := durationToString(d, false); got != "1.234567s" {
		t.Fatalf("durationToString(%v, false) = %q, want 1.234567s", d, got)
	}
	if got := durationToString(1500*time.Millisecond, true); got != "1.5" {
		t.Fatalf("durationToString(1.5s, true) = %q, want 1.5", got)
	}
}

func TestAlignBulkPadsColumns(t *testing.T) {
	bulk := [][]string{{"left", "1"}, {"much longer", "22"}}
	alignBulk(bulk, AlignLeft, AlignRight)

	if bulk[0][0] != "left       " {
		t.Fatalf("first column = %q, want left padded on the right", bulk[0][0])
	}
	if bulk[0][1] != " 1" {
		t.Fatalf("second column = %q, want right aligned", bulk[0][1])
	}
}

func testBreakdownSnapshotReport() *SnapshotReport {
	report := testSnapshotReport()
	report.Breakdown = []*PhaseReport{
		{
			Name: "Connect", Count: 2, Mean: 3 * time.Millisecond, Max: 4 * time.Millisecond,
			Percentiles: []time.Duration{3 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond},
		},
		{
			Name: "Wait", Count: 3, Mean: 12 * time.Millisecond, Max: 19 * time.Millisecond,
			Percentiles: []time.Duration{11 * time.Millisecond, 18 * time.Millisecond, 19 * time.Millisecond},
		},
	}
	return report
}

func TestPrinterFormatTableReportsIncludesBreakdown(t *testing.T) {
	printer := NewPrinter(3, 0, false, false)

	var without bytes.Buffer
	printer.formatTableReports(&without, testSnapshotReport(), true, false)
	if strings.Contains(without.String(), "Latency Breakdown") {
		t.Fatalf("breakdown table rendered without --breakdown:\n%s", without.String())
	}

	var with bytes.Buffer
	printer.formatTableReports(&with, testBreakdownSnapshotReport(), true, false)
	out := with.String()
	for _, want := range []string{"Latency Breakdown", "Connect", "Wait", "P50", "P90", "P99"} {
		if !strings.Contains(out, want) {
			t.Fatalf("breakdown table is missing %q:\n%s", want, out)
		}
	}
}

func TestPrinterFormatJSONReportsIncludesBreakdown(t *testing.T) {
	printer := NewPrinter(3, 0, false, false)
	var buf bytes.Buffer
	printer.formatJSONReports(&buf, testBreakdownSnapshotReport(), true, false)

	var got struct {
		Breakdown map[string]struct {
			Count int64  `json:"Count"`
			Mean  string `json:"Mean"`
			P50   string `json:"P50"`
			P99   string `json:"P99"`
			Max   string `json:"Max"`
		} `json:"Breakdown"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	wait, ok := got.Breakdown["Wait"]
	if !ok {
		t.Fatalf("Breakdown is missing the Wait phase: %s", buf.String())
	}
	if wait.Count != 3 || wait.Mean != "12ms" || wait.P50 != "11ms" || wait.P99 != "19ms" || wait.Max != "19ms" {
		t.Fatalf("Wait = %+v, want the phase report verbatim", wait)
	}
}
