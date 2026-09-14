/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * Offline tests for the DN_PERFORMANCE aggregation.
 *
 * These cover the pure logic - the DNAI-interval join, the usage-report
 * de-duplication key, the "omit, never zero" rule and the rate maths. The HTTP
 * handler itself needs MongoDB and is not covered here.
 *
 * Run:  go test ./internal/engine/
 */

package engine

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// upPathChDoc - one stored uppathchlist element, as the mongo driver decodes it
// (Go struct field names are lower-cased by the bson marshaller).
func upPathChDoc(source, target string, ts int64) bson.M {
	m := bson.M{"targetdnai": target, "timestamp": ts}
	if source != "" {
		m["sourcednai"] = source
	}
	return m
}

// ---------------------------------------------------------------------------
// A usage report is attributed to the DNAI whose interval contains its
// timestamp, across a steer that happens mid-window.
func TestDnaiIntervalJoinAcrossASteer(t *testing.T) {
	raw := primitive.A{
		// establishment: no sourceDnai, there is no previous DNAI
		upPathChDoc("", "internet-primary", 1000),
		// steer at t=1100
		upPathChDoc("internet-primary", "internet-secondary", 1100),
	}
	tl := buildDnaiTimeline(raw, 900, 1200, 0)

	cases := []struct {
		at   int64
		want string
	}{
		{950, ""},                     // before the session was known - unattributed
		{1000, "internet-primary"},    // exactly at establishment
		{1099, "internet-primary"},    // one second before the steer
		{1100, "internet-secondary"},  // exactly at the steer -> TARGET, not source
		{1200, "internet-secondary"},  // after
	}
	for _, c := range cases {
		if got := dnaiAt(tl, c.at); got != c.want {
			t.Errorf("dnaiAt(%d) = %q, want %q", c.at, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// A report BEFORE the first known notification is unattributed when there is no
// sourceDnai - it must never be guessed onto a DNAI.
func TestReportBeforeFirstNotificationIsUnattributed(t *testing.T) {
	raw := primitive.A{upPathChDoc("", "internet-primary", 1000)}
	tl := buildDnaiTimeline(raw, 900, 1200, 0)
	if got := dnaiAt(tl, 950); got != "" {
		t.Errorf("a report predating the only known DNAI was attributed to %q; "+
			"it must be left unattributed", got)
	}
}

// ...but when the earliest thing known IS a sourceDnai, TS 29.508 has told us
// where the session came from, so that interval legitimately covers the earlier
// part of the window - bounded by the window start, never indefinitely.
func TestSourceDnaiExtendsBackToWindowStartOnly(t *testing.T) {
	raw := primitive.A{upPathChDoc("internet-primary", "internet-secondary", 1100)}
	tl := buildDnaiTimeline(raw, 1000, 1200, 0)
	if got := dnaiAt(tl, 1050); got != "internet-primary" {
		t.Errorf("dnaiAt(1050) = %q, want internet-primary (from sourceDnai)", got)
	}
	if got := dnaiAt(tl, 999); got != "" {
		t.Errorf("dnaiAt(999) = %q, want \"\" - the extension must stop at the "+
			"window start, not run backwards forever", got)
	}
}

// A targetDnai-derived earliest interval must NOT be extended backwards: it says
// where the session WENT, not where it had been.
func TestTargetDnaiIsNotExtendedBackwards(t *testing.T) {
	raw := primitive.A{upPathChDoc("", "internet-primary", 1100)}
	tl := buildDnaiTimeline(raw, 1000, 1200, 0)
	if got := dnaiAt(tl, 1050); got != "" {
		t.Errorf("dnaiAt(1050) = %q, want \"\" - a targetDnai must not be "+
			"extended backwards", got)
	}
}

// Entries after the end of the window describe a path the window never saw.
func TestEntriesAfterTheWindowAreExcluded(t *testing.T) {
	raw := primitive.A{
		upPathChDoc("", "internet-primary", 1000),
		upPathChDoc("internet-primary", "internet-secondary", 5000),
	}
	tl := buildDnaiTimeline(raw, 900, 1200, 0)
	for _, iv := range tl {
		if iv.dnai == "internet-secondary" {
			t.Fatalf("an entry at t=5000 leaked into a window ending at 1200")
		}
	}
	if got := dnaiAt(tl, 1200); got != "internet-primary" {
		t.Errorf("dnaiAt(1200) = %q, want internet-primary", got)
	}
}

// An entry with no targetDnai carries no information and must be dropped.
func TestEntryWithoutTargetDnaiIsDropped(t *testing.T) {
	raw := primitive.A{
		bson.M{"timestamp": int64(1000)},
		upPathChDoc("", "internet-primary", 1010),
	}
	tl := buildDnaiTimeline(raw, 900, 1200, 0)
	if len(tl) != 1 || tl[0].dnai != "internet-primary" {
		t.Errorf("timeline = %+v, want exactly the one usable entry", tl)
	}
}

// ---------------------------------------------------------------------------
// De-duplication: two stored elements with the same (seid, urseqn) are the same
// PFCP usage report, however many stale subscriptions produced them.
func TestUsageReportDedupKey(t *testing.T) {
	a := bson.M{"seid": int64(1), "urseqn": int32(498)}
	b := bson.M{"seid": int32(1), "urseqn": int64(498)} // driver may pick either width
	ka, okA := usageReportKeyOf(a)
	kb, okB := usageReportKeyOf(b)
	if !okA || !okB {
		t.Fatalf("key extraction failed: okA=%v okB=%v", okA, okB)
	}
	if ka != kb {
		t.Errorf("the same report decoded at different integer widths produced "+
			"different keys: %+v vs %+v", ka, kb)
	}
	if _, ok := usageReportKeyOf(bson.M{"seid": int64(1)}); ok {
		t.Error("a report with no urseqn must not yield a dedup key - the " +
			"caller has to fall back to counting it")
	}
}

// ---------------------------------------------------------------------------
// Rate maths: avgTrafficRate divides by the SUM OF THE REPORTS' OWN DURATIONS,
// never by the poll window; maxTrafficRate uses a single report's own duration.
func TestRateMathsUsesTheReportsOwnDuration(t *testing.T) {
	// two reports: 1000 bytes over 10 s, then 4000 bytes over 10 s
	//   avg = (1000+4000)*8 / (10+10) = 2000 bps
	//   max = 4000*8/10             = 3200 bps
	a := &dnPerfAccum{
		volBits:    (1000 + 4000) * 8,
		durSec:     20,
		maxRateBps: float64(4000*8) / 10,
		samples:    2,
		firstTs:    1000,
		lastTs:     1010,
	}
	accum := map[dnPerfKey]*dnPerfAccum{
		{dnn: "default", sst: 222, sd: "000005", dnai: "internet-primary"}: a,
	}
	out := buildDnPerfResponse(accum, "upf-1", EngineReqData{})
	if len(out) != 1 || len(out[0].DnPerf) != 1 {
		t.Fatalf("out = %+v, want one group with one entry", out)
	}
	e := out[0].DnPerf[0]
	if e.PerfData.AvgTrafficRate != "2.0 Kbps" {
		t.Errorf("AvgTrafficRate = %q, want \"2.0 Kbps\"", e.PerfData.AvgTrafficRate)
	}
	if e.PerfData.MaxTrafficRate != "3.2 Kbps" {
		t.Errorf("MaxTrafficRate = %q, want \"3.2 Kbps\"", e.PerfData.MaxTrafficRate)
	}
	if e.UpfId != "upf-1" || e.Dnai != "internet-primary" {
		t.Errorf("entry lost its identity: upfId=%q dnai=%q", e.UpfId, e.Dnai)
	}
	// Absent, never faked: the unmeasurable fields are not even modelled, so
	// there is nothing that could serialise as zero.
	if e.TemporalValidCon == nil {
		t.Error("Table 6.14.3-1 '> Temporal Validity Condition' is missing")
	}
}

// A DNAI with no contributing reports is OMITTED, not reported as 0 bps.
func TestDnaiWithNoReportsIsOmitted(t *testing.T) {
	accum := map[dnPerfKey]*dnPerfAccum{
		{dnn: "default", dnai: "internet-primary"}:   {volBits: 800, durSec: 10, samples: 1},
		{dnn: "default", dnai: "internet-secondary"}: {volBits: 0, durSec: 0, samples: 0},
	}
	out := buildDnPerfResponse(accum, "upf-1", EngineReqData{})
	for _, info := range out {
		for _, e := range info.DnPerf {
			if e.Dnai == "internet-secondary" {
				t.Errorf("a DNAI with no reports was reported as %q; it must be "+
					"omitted - zero would mean 'measured zero'",
					e.PerfData.AvgTrafficRate)
			}
		}
	}
}

// A measured zero IS reportable - it is different from "no reports at all".
func TestMeasuredZeroIsReported(t *testing.T) {
	accum := map[dnPerfKey]*dnPerfAccum{
		{dnn: "default", dnai: "internet-primary"}: {volBits: 0, durSec: 10, samples: 1},
	}
	out := buildDnPerfResponse(accum, "upf-1", EngineReqData{})
	if len(out) != 1 || len(out[0].DnPerf) != 1 {
		t.Fatalf("a DNAI with reports but zero volume must still be reported; got %+v", out)
	}
	if got := out[0].DnPerf[0].PerfData.AvgTrafficRate; got != "0 bps" {
		t.Errorf("AvgTrafficRate = %q, want \"0 bps\"", got)
	}
}

// ---------------------------------------------------------------------------
// Table 6.14.1-1 filters. An empty filter is "no filter"; a non-empty one
// matches only its own members.
func TestTableFilters(t *testing.T) {
	accum := map[dnPerfKey]*dnPerfAccum{
		{dnn: "default", sst: 222, sd: "000005", dnai: "internet-primary"}:   {volBits: 800, durSec: 10, samples: 1},
		{dnn: "default", sst: 222, sd: "000005", dnai: "internet-secondary"}: {volBits: 1600, durSec: 10, samples: 1},
	}
	count := func(req EngineReqData) int {
		n := 0
		for _, info := range buildDnPerfResponse(accum, "upf-1", req) {
			n += len(info.DnPerf)
		}
		return n
	}
	if got := count(EngineReqData{}); got != 2 {
		t.Errorf("no filter returned %d entries, want 2", got)
	}
	if got := count(EngineReqData{Dnais: []string{"internet-secondary"}}); got != 1 {
		t.Errorf("dnais filter returned %d entries, want 1", got)
	}
	if got := count(EngineReqData{Dnais: []string{"does-not-exist"}}); got != 0 {
		t.Errorf("an unknown DNAI returned %d entries, want 0", got)
	}
	if got := count(EngineReqData{Dnns: []string{"default"}}); got != 2 {
		t.Errorf("matching dnn filter returned %d entries, want 2", got)
	}
	if got := count(EngineReqData{Dnns: []string{"other"}}); got != 0 {
		t.Errorf("non-matching dnn filter returned %d entries, want 0", got)
	}
	// S-NSSAI: SD is compared only when the filter carries one.
	if got := count(EngineReqData{Snssaia: []Snssai{{Sst: 222}}}); got != 2 {
		t.Errorf("sst-only filter returned %d entries, want 2", got)
	}
	if got := count(EngineReqData{Snssaia: []Snssai{{Sst: 222, Sd: "000005"}}}); got != 2 {
		t.Errorf("sst+sd filter returned %d entries, want 2", got)
	}
	if got := count(EngineReqData{Snssaia: []Snssai{{Sst: 1}}}); got != 0 {
		t.Errorf("non-matching sst returned %d entries, want 0", got)
	}
}

// Output order must be deterministic: a consumer ranking two DNAIs must not see
// them swap places between identical requests (Go randomises map iteration).
func TestOutputOrderIsDeterministic(t *testing.T) {
	accum := map[dnPerfKey]*dnPerfAccum{
		{dnn: "default", dnai: "internet-secondary"}: {volBits: 1600, durSec: 10, samples: 1},
		{dnn: "default", dnai: "internet-primary"}:   {volBits: 800, durSec: 10, samples: 1},
		{dnn: "other", dnai: "internet-primary"}:     {volBits: 800, durSec: 10, samples: 1},
	}
	var first []string
	for i := 0; i < 20; i++ {
		var got []string
		for _, info := range buildDnPerfResponse(accum, "upf-1", EngineReqData{}) {
			for _, e := range info.DnPerf {
				got = append(got, info.Dnn+"/"+e.Dnai)
			}
		}
		if i == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("iteration %d returned %d entries, first returned %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("iteration %d order %v differs from %v", i, got, first)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// PREDICTIONS - TS 23.288 Table 6.14.3-2.
//
// These pin down the two things TS 23.288 leaves to the producer, so that the
// definitions in predictDnPerf() cannot drift silently.

// Fewer than three points yields NO prediction, rather than one with a token
// confidence. Absent, never faked.
func TestPredictionRefusedOnTooFewSamples(t *testing.T) {
	for _, n := range []int{0, 1, 2} {
		a := &dnPerfAccum{firstTs: 1000}
		for i := 0; i < n; i++ {
			a.series = append(a.series, ratePoint{ts: int64(1000 + i*10), bps: 1e6})
		}
		if _, _, ok := predictDnPerf(a, 60, 1100); ok {
			t.Errorf("%d sample(s) produced a prediction; it must be omitted", n)
		}
	}
}

// A flat series extrapolates to itself.
func TestPredictionOfAFlatSeries(t *testing.T) {
	a := &dnPerfAccum{firstTs: 1000}
	for i := 0; i < 10; i++ {
		a.series = append(a.series, ratePoint{ts: int64(1000 + i*10), bps: 5e6})
	}
	got, conf, ok := predictDnPerf(a, 60, 1090)
	if !ok {
		t.Fatal("a 10-point series must be predictable")
	}
	if got < 4.9e6 || got > 5.1e6 {
		t.Errorf("flat series predicted %.0f bps, want ~5000000", got)
	}
	// Perfectly stable and reasonably sampled: confidence should be high, and
	// must still respect the documented cap of 90.
	if conf < 50 || conf > 90 {
		t.Errorf("confidence %d for a flat 10-point series, want 50..90", conf)
	}
}

// A rising series predicts ABOVE its own mean - that is the point of a trend.
func TestPredictionFollowsATrend(t *testing.T) {
	a := &dnPerfAccum{firstTs: 1000}
	var sum float64
	for i := 0; i < 10; i++ {
		bps := float64(1e6 * (i + 1)) // 1,2,...,10 Mbit/s
		a.series = append(a.series, ratePoint{ts: int64(1000 + i*10), bps: bps})
		sum += bps
	}
	mean := sum / 10
	got, _, ok := predictDnPerf(a, 60, 1090)
	if !ok {
		t.Fatal("expected a prediction")
	}
	if got <= mean {
		t.Errorf("rising series predicted %.0f bps, which is not above the mean %.0f", got, mean)
	}
}

// A falling series must never predict a negative rate.
func TestPredictionIsClampedAtZero(t *testing.T) {
	a := &dnPerfAccum{firstTs: 1000}
	for i := 0; i < 10; i++ {
		bps := float64(10e6 - i*1e6) // 10 Mbit/s falling to 1
		a.series = append(a.series, ratePoint{ts: int64(1000 + i*10), bps: bps})
	}
	// Extrapolating far past the end of a steep decline goes negative
	// arithmetically; a negative traffic rate is not a thing.
	got, _, ok := predictDnPerf(a, 6000, 1090)
	if !ok {
		t.Fatal("expected a prediction")
	}
	if got < 0 {
		t.Errorf("predicted %.0f bps; a rate must never be negative", got)
	}
}

// Confidence must FALL as the series becomes more dispersed. This is the
// property that stops a wildly varying path being acted on.
func TestConfidenceFallsWithDispersion(t *testing.T) {
	stable := &dnPerfAccum{firstTs: 1000}
	noisy := &dnPerfAccum{firstTs: 1000}
	for i := 0; i < 12; i++ {
		stable.series = append(stable.series, ratePoint{ts: int64(1000 + i*10), bps: 5e6})
		bps := 1e6
		if i%2 == 0 {
			bps = 9e6
		}
		noisy.series = append(noisy.series, ratePoint{ts: int64(1000 + i*10), bps: bps})
	}
	_, cStable, _ := predictDnPerf(stable, 60, 1110)
	_, cNoisy, _ := predictDnPerf(noisy, 60, 1110)
	if !(cNoisy < cStable) {
		t.Errorf("noisy series confidence %d is not below stable series confidence %d",
			cNoisy, cStable)
	}
}

// Confidence must RISE with sample count, all else equal.
func TestConfidenceRisesWithSampleCount(t *testing.T) {
	few := &dnPerfAccum{firstTs: 1000}
	many := &dnPerfAccum{firstTs: 1000}
	for i := 0; i < 3; i++ {
		few.series = append(few.series, ratePoint{ts: int64(1000 + i*10), bps: 5e6})
	}
	for i := 0; i < 30; i++ {
		many.series = append(many.series, ratePoint{ts: int64(1000 + i*10), bps: 5e6})
	}
	_, cFew, _ := predictDnPerf(few, 60, 1300)
	_, cMany, _ := predictDnPerf(many, 60, 1300)
	if !(cFew < cMany) {
		t.Errorf("3-sample confidence %d is not below 30-sample confidence %d", cFew, cMany)
	}
	if cMany > 90 {
		t.Errorf("confidence %d exceeds the documented cap of 90", cMany)
	}
}

// Statistics must carry NO confidence; predictions must carry one.
func TestConfidenceOnlyAppearsForPredictions(t *testing.T) {
	mk := func() map[dnPerfKey]*dnPerfAccum {
		a := &dnPerfAccum{volBits: 8e7, durSec: 10, samples: 6, firstTs: 1000, lastTs: 1050}
		for i := 0; i < 6; i++ {
			a.series = append(a.series, ratePoint{ts: int64(1000 + i*10), bps: 8e6})
		}
		return map[dnPerfKey]*dnPerfAccum{
			{dnn: "default", dnai: "internet-primary"}: a,
		}
	}
	stats := buildDnPerfResponse(mk(), "upf-1", EngineReqData{})
	if len(stats) != 1 {
		t.Fatalf("expected one group, got %d", len(stats))
	}
	if stats[0].Confidence != 0 {
		t.Errorf("statistics carried confidence %d; Table 6.14.3-1 defines none",
			stats[0].Confidence)
	}
	if stats[0].DnPerf[0].Predicted {
		t.Error("statistics entry marked as predicted")
	}

	preds := buildDnPerfResponse(mk(), "upf-1", EngineReqData{OffsetPeriod: 60})
	if len(preds) != 1 {
		t.Fatalf("expected one group, got %d", len(preds))
	}
	if preds[0].Confidence <= 0 {
		t.Errorf("prediction carried no confidence; Table 6.14.3-2 requires one")
	}
	if !preds[0].DnPerf[0].Predicted {
		t.Error("prediction entry not marked as predicted")
	}
}

// A DNAI with too few samples is OMITTED from a prediction rather than being
// emitted with a made-up confidence - even when a sibling DNAI is predictable.
func TestThinDnaiOmittedFromPrediction(t *testing.T) {
	rich := &dnPerfAccum{volBits: 8e7, durSec: 10, samples: 8, firstTs: 1000, lastTs: 1070}
	for i := 0; i < 8; i++ {
		rich.series = append(rich.series, ratePoint{ts: int64(1000 + i*10), bps: 8e6})
	}
	thin := &dnPerfAccum{volBits: 8e6, durSec: 10, samples: 1, firstTs: 1000, lastTs: 1000}
	thin.series = append(thin.series, ratePoint{ts: 1000, bps: 8e6})

	accum := map[dnPerfKey]*dnPerfAccum{
		{dnn: "default", dnai: "internet-primary"}:   rich,
		{dnn: "default", dnai: "internet-secondary"}: thin,
	}
	out := buildDnPerfResponse(accum, "upf-1", EngineReqData{OffsetPeriod: 60})
	for _, info := range out {
		for _, e := range info.DnPerf {
			if e.Dnai == "internet-secondary" {
				t.Error("a DNAI with one sample was predicted; it must be omitted")
			}
		}
	}
	// ...but it IS reported as a statistic, where one sample is a real
	// measurement.
	out = buildDnPerfResponse(accum, "upf-1", EngineReqData{})
	found := false
	for _, info := range out {
		for _, e := range info.DnPerf {
			if e.Dnai == "internet-secondary" {
				found = true
			}
		}
	}
	if !found {
		t.Error("a one-sample DNAI must still appear in STATISTICS")
	}
}

// A prediction must never exceed the highest rate the path has actually
// achieved. Extrapolating a rising trend otherwise yields rates the path has
// never demonstrated - seen live as 1.20 Gbps predicted against a 340 Mbps
// observed maximum.
func TestPredictionNeverExceedsObservedMaximum(t *testing.T) {
	a := &dnPerfAccum{firstTs: 1000}
	var maxSeen float64
	for i := 0; i < 12; i++ {
		bps := float64(1e6 * (i + 1)) // steeply rising, 1..12 Mbit/s
		a.series = append(a.series, ratePoint{ts: int64(1000 + i*10), bps: bps})
		if bps > maxSeen {
			maxSeen = bps
		}
	}
	a.maxRateBps = maxSeen
	// Extrapolate a long way ahead, where an unclamped fit would run away.
	got, _, ok := predictDnPerf(a, 3600, 1110)
	if !ok {
		t.Fatal("expected a prediction")
	}
	if got > maxSeen {
		t.Errorf("predicted %.0f bps, above the observed maximum %.0f - a "+
			"prediction must stay within what the path has demonstrated",
			got, maxSeen)
	}
}

// ---------------------------------------------------------------------------
// ENGINE_DN_PERFORMANCE_WINDOW_SEC - the default analytics window is
// configurable because it dominates closed-loop reaction latency. Three
// properties matter and are asserted here:
// the shipped default is unchanged at 300 so an unset variable changes no
// behaviour; a configured value is honoured; and a non-positive value falls
// back to 300 rather than being read as "no bound", which would reintroduce
// the unbounded scan applyDefaultRecentWindow exists to prevent.
func TestDnPerformanceWindowSecDefaultsTo300(t *testing.T) {
	saved := config.DnPerformance.WindowSec
	defer func() { config.DnPerformance.WindowSec = saved }()

	// envconfig's `default:"300"` supplies 300 at load time; the zero value
	// here stands for a config that was never populated at all.
	config.DnPerformance.WindowSec = 0
	if got := dnPerformanceWindowSec(); got != 300 {
		t.Errorf("unset window = %d, want the 300 s default", got)
	}
}

func TestDnPerformanceWindowSecHonoursConfiguredValue(t *testing.T) {
	saved := config.DnPerformance.WindowSec
	defer func() { config.DnPerformance.WindowSec = saved }()

	for _, want := range []int64{30, 60, 120, 900} {
		config.DnPerformance.WindowSec = want
		if got := dnPerformanceWindowSec(); got != want {
			t.Errorf("configured window %d, got %d", want, got)
		}
	}
}

func TestDnPerformanceWindowSecRejectsNonPositive(t *testing.T) {
	saved := config.DnPerformance.WindowSec
	defer func() { config.DnPerformance.WindowSec = saved }()

	for _, bad := range []int64{-1, -300} {
		config.DnPerformance.WindowSec = bad
		if got := dnPerformanceWindowSec(); got != 300 {
			t.Errorf("window %d must fall back to 300, got %d - a "+
				"non-positive value must never be read as 'unbounded'",
				bad, got)
		}
	}
}

// The window is what applyDefaultRecentWindow is given, and it must only apply
// when the CONSUMER supplied no window of its own. A consumer-supplied window
// is a TS 29.520 request parameter and outranks local configuration.
func TestConfiguredWindowNeverOverridesAConsumerSuppliedOne(t *testing.T) {
	saved := config.DnPerformance.WindowSec
	defer func() { config.DnPerformance.WindowSec = saved }()
	config.DnPerformance.WindowSec = 30

	start := time.Unix(1000, 0)
	end := time.Unix(9000, 0)
	req := EngineReqData{StartTs: start, EndTs: end}
	applyDefaultRecentWindow(&req, dnPerformanceWindowSec())
	if !req.StartTs.Equal(start) || !req.EndTs.Equal(end) {
		t.Errorf("consumer window %v..%v was overwritten with %v..%v",
			start, end, req.StartTs, req.EndTs)
	}

	// ...and with no consumer window, the configured one is what gets applied.
	empty := EngineReqData{}
	applyDefaultRecentWindow(&empty, dnPerformanceWindowSec())
	span := empty.EndTs.Sub(empty.StartTs).Seconds()
	if span < 29 || span > 31 {
		t.Errorf("applied window span = %.0fs, want ~30s", span)
	}
}

// ---------------------------------------------------------------------------
// The 18.7 ambiguity detector. It must fire ONLY on the case the time join
// cannot resolve - several PDU sessions of one SUPI whose reports land on
// several DNAIs - and must stay quiet on the two innocent cases, particularly
// the post-restart one, or it would warn on every redeploy.
func TestAttributionAmbiguity(t *testing.T) {
	seids := func(v ...int64) map[int64]bool {
		m := map[int64]bool{}
		for _, x := range v {
			m[x] = true
		}
		return m
	}
	dnais := func(v ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range v {
			m[x] = true
		}
		return m
	}
	cases := []struct {
		name  string
		s     map[int64]bool
		d     map[string]bool
		want  bool
		why   string
	}{
		{"one session, one path", seids(1), dnais("a"), false,
			"the ordinary case"},
		{"one session steered across paths", seids(1), dnais("a", "b"), false,
			"a single session moving is exactly what the time join handles"},
		{"successive sessions on one path", seids(1, 2), dnais("a"), false,
			"an SMF restart renumbers seids; same path either way"},
		{"concurrent sessions across paths", seids(1, 2), dnais("a", "b"), true,
			"the only case where a report may be on the wrong path"},
		{"nothing at all", seids(), dnais(), false, "no data, no claim"},
	}
	for _, c := range cases {
		if got := attributionIsAmbiguous(c.s, c.d); got != c.want {
			t.Errorf("%s: got %v, want %v - %s", c.name, got, c.want, c.why)
		}
	}
}
