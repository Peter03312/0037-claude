package align

import (
	"math"
	"strings"
	"testing"
)

func mkDataset(times []int64, trainPipe, brakeCylinder []float64) *Dataset {
	return &Dataset{
		Times: times,
		Cols: map[string][]float64{
			ColTrainPipe:     trainPipe,
			ColBrakeCylinder: brakeCylinder,
		},
		NSamp: len(times),
	}
}

func uniformTimes(n int, step int64) []int64 {
	ts := make([]int64, n)
	for i := range ts {
		ts[i] = int64(i) * step
	}
	return ts
}

func constVals(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func mustSpec(tb interface {
	Helper()
	Fatalf(string, ...any)
}, js string) *Spec {
	tb.Helper()
	spec, errs := ParseSpec([]byte(js))
	if len(errs) != 0 {
		tb.Fatalf("spec rejected unexpectedly: %v", errs)
	}
	return spec
}

func approxEq(x, y, tol float64) bool { return math.Abs(x-y) <= tol }

// ---------------------------------------------------------------------------
// 1) 共享边界：相邻阶段恰好共享一个边界样本，首阶段始于首行，末阶段止于末行。
// ---------------------------------------------------------------------------

func TestAlignQualifiedSharedBoundaries(t *testing.T) {
	// 局部贪心陷阱数据（见 TestAlignGreedyTrap 的构造）。
	times := uniformTimes(9, 100)
	tp := []float64{450, 500, 550, 600, 600, 600, 540, 490, 440}
	ds := mkDataset(times, tp, constVals(9, 0))
	spec := greedyTrapSpec(t)

	res := Align(ds, spec)
	if res.Verdict != "qualified" {
		t.Fatalf("want qualified, got %s diagnosis=%+v", res.Verdict, res.Diagnosis)
	}
	if got := res.EndBoundaryVector; len(got) != 3 || got[0] != 3 || got[1] != 5 || got[2] != 8 {
		t.Fatalf("end boundary vector = %v, want [3 5 8]", got)
	}
	if len(res.Phases) != 3 {
		t.Fatalf("want 3 phase reports, got %d", len(res.Phases))
	}
	for i := 1; i < 3; i++ {
		if res.Phases[i].StartSample != res.Phases[i-1].EndSample {
			t.Fatalf("phases %d and %d do not share boundary: %d != %d",
				i-1, i, res.Phases[i].StartSample, res.Phases[i-1].EndSample)
		}
		if res.Phases[i].StartMS != res.Phases[i-1].EndMS {
			t.Fatalf("shared boundary times differ")
		}
	}
	if res.Phases[0].StartSample != 0 || res.Phases[0].StartCSVRow != 2 {
		t.Fatalf("first phase must start on first data row, got %+v", res.Phases[0])
	}
	if res.Phases[2].EndSample != 8 || res.Phases[2].EndCSVRow != 10 {
		t.Fatalf("last phase must end on last data row, got %+v", res.Phases[2])
	}
	// 每个内部样本只归一个阶段：阶段长度之和（间隔数）== 全周期间隔数。
	totalIntervals := 0
	for _, p := range res.Phases {
		totalIntervals += p.EndSample - p.StartSample
	}
	if totalIntervals != ds.NSamp-1 {
		t.Fatalf("sample intervals double-counted or missing: %d != %d", totalIntervals, ds.NSamp-1)
	}
}

// ---------------------------------------------------------------------------
// 2) 插值越带：交点落在样本之间，用分段线性插值求单次/累计越带时长。
// ---------------------------------------------------------------------------

func TestSegmentExcursionsInterpolation(t *testing.T) {
	// 0ms:110 -> 300ms:101，带 [95,105]，上界交点 fr=(105-110)/(101-110)=5/9 => t=166.67ms
	outs := segmentExcursions(0, 300, 110, 101, 95, 105)
	if len(outs) != 1 {
		t.Fatalf("want one excursion interval, got %v", outs)
	}
	if !approxEq(outs[0][0], 0, 1e-9) || !approxEq(outs[0][1], 300.0*5.0/9.0, 1e-6) {
		t.Fatalf("interpolated excursion = %v, want [0 ~166.67]", outs[0])
	}

	// 两端都在带内、中间穿出：100 -> 110 -> 100，带 [95,105]，t=0,100,200。
	st := holdState{pendingStart: -1}
	st = growHold(mkDataset([]int64{0, 100, 200}, []float64{100, 110, 100}, constVals(3, 0)),
		PhaseSpec{Kind: KindHolding, ControlledColumn: ColTrainPipe, TargetFinal: 100, HoldingBandHalfWidth: 5},
		st, 0)
	st = growHold(mkDataset([]int64{0, 100, 200}, []float64{100, 110, 100}, constVals(3, 0)),
		PhaseSpec{Kind: KindHolding, ControlledColumn: ColTrainPipe, TargetFinal: 100, HoldingBandHalfWidth: 5},
		st, 1)
	m := st.snapshot()
	if !approxEq(m.single, 100, 1e-9) || !approxEq(m.total, 100, 1e-9) {
		t.Fatalf("middle excursion single/total = %v/%v, want 100/100", m.single, m.total)
	}
}

func TestExcursionSplitAndMergeAcrossSamples(t *testing.T) {
	// 110 -> 100 -> 110：两段越带中间夹着严格带内区间（交点 50ms、150ms），
	// 必须拆成两次：single=50, total=100。
	ds := mkDataset([]int64{0, 100, 200}, []float64{110, 100, 110}, constVals(3, 0))
	p := PhaseSpec{Kind: KindHolding, ControlledColumn: ColTrainPipe, TargetFinal: 100, HoldingBandHalfWidth: 5}
	m := computeHoldMetrics(ds, p, 0, 2)
	if !approxEq(m.single, 50, 1e-9) || !approxEq(m.total, 100, 1e-9) {
		t.Fatalf("split: single/total = %v/%v, want 50/100", m.single, m.total)
	}

	// 110 -> 110 -> 100：在带外样本点相接，必须合并成一次连续越带 150ms。
	ds2 := mkDataset([]int64{0, 100, 200}, []float64{110, 110, 100}, constVals(3, 0))
	m2 := computeHoldMetrics(ds2, p, 0, 2)
	if !approxEq(m2.single, 150, 1e-9) || !approxEq(m2.total, 150, 1e-9) {
		t.Fatalf("merge: single/total = %v/%v, want 150/150", m2.single, m2.total)
	}

	// 界点相接也算连续：110 -> 105 -> 100，第二交点恰在 t=100 界点，越带 100ms 整段连续。
	ds3 := mkDataset([]int64{0, 100, 200}, []float64{110, 105, 100}, constVals(3, 0))
	m3 := computeHoldMetrics(ds3, p, 0, 2)
	if !approxEq(m3.single, 100, 1e-9) || !approxEq(m3.total, 100, 1e-9) {
		t.Fatalf("touch: single/total = %v/%v, want 100/100", m3.single, m3.total)
	}
}

func TestAlignHoldingInterpolationEndToEnd(t *testing.T) {
	// 110 -> 100 -> 100，上界 105 的交点恰在 500ms，越带恰好 500ms（闭区间预算相等应合格）。
	times := []int64{0, 1000, 2000}
	tp := []float64{110, 100, 100}
	ds := mkDataset(times, tp, constVals(3, 0))
	js := `{
	  "phases": [
	    {"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":1000,"max_ms":1000},
	     "target_final":100,"abs_tolerance":5,
	     "holding_band_half_width":5,"max_single_out_of_band_ms":500,"max_total_out_of_band_ms":500,
	     "max_sample_gap_ms":2000},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":1000,"max_ms":1000},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":2000}
	  ]}`
	res := Align(ds, mustSpec(t, js))
	if res.Verdict != "qualified" {
		t.Fatalf("want qualified at exact excursion budget, got %+v", res.Diagnosis)
	}
	if !approxEq(res.Phases[0].SingleExcursionMS, 500, 1e-6) {
		t.Fatalf("interpolated excursion = %v, want 500", res.Phases[0].SingleExcursionMS)
	}

	// 非整数交点：0..300ms 内 110 -> 101，越带 166.67ms；预算 166ms 时不合格。
	ds2 := mkDataset([]int64{0, 300, 600}, []float64{110, 101, 101}, constVals(3, 0))
	js2 := `{
	  "phases": [
	    {"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":300,"max_ms":300},
	     "target_final":100,"abs_tolerance":5,
	     "holding_band_half_width":5,"max_single_out_of_band_ms":166,"max_total_out_of_band_ms":166,
	     "max_sample_gap_ms":1000},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":300,"max_ms":300},
	     "target_final":101,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":1000}
	  ]}`
	res2 := Align(ds2, mustSpec(t, js2))
	if res2.Verdict != "rejected" {
		t.Fatalf("want rejected when interpolated 166.67ms exceeds 166ms budget, got qualified")
	}
	if res2.Diagnosis.Violation.Code != ViolSingleExcurs {
		t.Fatalf("want %s, got %s", ViolSingleExcurs, res2.Diagnosis.Violation.Code)
	}
	if !approxEq(res2.Diagnosis.Violation.Actual, 300.0*5.0/9.0, 1e-6) {
		t.Fatalf("reported excursion actual = %v", res2.Diagnosis.Violation.Actual)
	}
}

// ---------------------------------------------------------------------------
// 3) 局部贪心失败：逐段取首个局部匹配会消耗后续唯一边界；DP 必须找到全局切分。
//
//	数据（间隔 100ms，受控列 train_pipe）：
//	  idx:  0    1    2    3    4    5    6    7    8
//	  v:   450  500  550  600  600  600  540  490  440
//	充气合法右端有 e=2（首个局部匹配）与 e=3：
//	  - 贪心取 e=2 后，保压从样本 2（550）起步，进入 [590,610] 需 80ms，
//	    超过单次越带预算 50ms，保压无任何合法右端 → 合格阀被误判；
//	  - DP 枚举全部候选后取 e=3，保压 [3,5]、缓解 [5,8]，整周期合格。
//	保压取 e=4 同样会使缓解闭区间 [300,300]ms 落空，构成第二层陷阱。
// ---------------------------------------------------------------------------

func greedyTrapSpec(t *testing.T) *Spec {
	return mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":300},
	     "target_final":550,"abs_tolerance":60,"allowed_reverse_cumulative":1,
	     "max_sample_gap_ms":200},
	    {"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":200},
	     "target_final":600,"abs_tolerance":5,
	     "holding_band_half_width":10,"max_single_out_of_band_ms":50,"max_total_out_of_band_ms":50,
	     "max_sample_gap_ms":200},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":300,"max_ms":300},
	     "target_final":440,"abs_tolerance":5,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200}
	  ]}`)
}

func TestAlignGreedyTrap(t *testing.T) {
	times := uniformTimes(9, 100)
	tp := []float64{450, 500, 550, 600, 600, 600, 540, 490, 440}
	ds := mkDataset(times, tp, constVals(9, 0))

	// 先固化贪心前提：充气的首个合法右端确实是 e=2。
	spec := greedyTrapSpec(t)
	pf := buildPrefix(ds, spec.Phases[0])
	firstValid := -1
	candidateIter(ds, spec.Phases[0], pf, 0, false, func(ev Eval) {
		if firstValid == -1 && ev.Valid {
			firstValid = ev.End
		}
	})
	if firstValid != 2 {
		t.Fatalf("test premise broken: first valid charging end = %d, want 2", firstValid)
	}
	// 且保压从样本 2 出发确实没有合法右端（贪心必败）。
	anyValid := false
	pf1 := buildPrefix(ds, spec.Phases[1])
	candidateIter(ds, spec.Phases[1], pf1, 2, false, func(ev Eval) {
		if ev.Valid {
			anyValid = true
		}
	})
	if anyValid {
		t.Fatalf("test premise broken: holding unexpectedly valid from sample 2")
	}

	res := Align(ds, spec)
	if res.Verdict != "qualified" {
		t.Fatalf("global DP must recover the valve, got rejected: %+v", res.Diagnosis)
	}
	if got := res.EndBoundaryVector; got[0] != 3 || got[1] != 5 || got[2] != 8 {
		t.Fatalf("vector = %v, want [3 5 8]", got)
	}
}

// 无完整切分时不得给部分合格：末阶段终值无法达标 → rejected + 诊断路径。
func TestAlignRejectedDiagnosisNoPartial(t *testing.T) {
	times := uniformTimes(9, 100)
	tp := []float64{450, 500, 550, 600, 600, 600, 540, 490, 440}
	ds := mkDataset(times, tp, constVals(9, 0))
	spec := greedyTrapSpec(t)
	// 放宽缓解闭区间（[4,8]=400ms 与 [5,8]=300ms 均可），再把目标改成数据无法达到的值。
	spec.Phases[2].Duration = ClosedInterval{Min: 300, Max: 400}
	spec.Phases[2].TargetFinal = 430
	spec.Phases[2].AbsTolerance = 1

	res := Align(ds, spec)
	if res.Verdict != "rejected" {
		t.Fatalf("want rejected, got %s", res.Verdict)
	}
	if len(res.Phases) != 0 {
		t.Fatalf("must not report partial phase results, got %d", len(res.Phases))
	}
	d := res.Diagnosis
	if d == nil {
		t.Fatal("missing diagnosis")
	}
	if d.CompletedPhases != 2 {
		t.Fatalf("completed phases = %d, want 2", d.CompletedPhases)
	}
	// 前两层合法路径向量为 [3,4] 与 [3,5]，字典序最小者 [3,4]。
	if got := d.EndBoundaryVector; len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("diagnosis vector = %v, want [3 4]", got)
	}
	if d.NextPhaseIndex != 2 || d.NextStartSample != 4 {
		t.Fatalf("next phase = %d @ sample %d, want 2 @ 4", d.NextPhaseIndex, d.NextStartSample)
	}
	if d.Violation.Code != ViolFinalValue {
		t.Fatalf("violation = %s, want %s", d.Violation.Code, ViolFinalValue)
	}
	if d.Violation.Interval.StartSample != 4 || d.Violation.Interval.EndSample != 8 {
		t.Fatalf("violation interval = %+v, want [4,8]", d.Violation.Interval)
	}
}

// ---------------------------------------------------------------------------
// 4) 采样断口：候选内相邻采样间隔超限时候选无效；诊断给出断口区间。
// ---------------------------------------------------------------------------

func TestSampleGapInvalidatesCandidate(t *testing.T) {
	// 样本 2->3 间隔 500ms，超过 150ms 上限；单阶段必须覆盖整单，故必失败。
	times := []int64{0, 100, 200, 700, 800, 900}
	tp := []float64{100, 100, 100, 100, 100, 100}
	ds := mkDataset(times, tp, constVals(6, 0))
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":2000},
	     "target_final":100,"abs_tolerance":0,
	     "holding_band_half_width":1,"max_single_out_of_band_ms":0,"max_total_out_of_band_ms":0,
	     "max_sample_gap_ms":150}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "rejected" {
		t.Fatalf("want rejected due to gap, got %s", res.Verdict)
	}
	v := res.Diagnosis.Violation
	if v.Code != ViolSampleGap {
		t.Fatalf("code = %s, want sample_gap", v.Code)
	}
	if len(v.Gaps) != 1 || v.Gaps[0].FromSample != 2 || v.Gaps[0].ToSample != 3 ||
		v.Gaps[0].ActualMS != 500 || v.Gaps[0].LimitMS != 150 {
		t.Fatalf("gap report = %+v, want [2->3] 500/150", v.Gaps)
	}
	if v.Interval.StartSample != 0 || v.Interval.EndSample != 5 {
		t.Fatalf("violation interval = %+v, want whole cycle [0,5]", v.Interval)
	}
}

// ---------------------------------------------------------------------------
// 5) 多解择优：先比各阶段终值绝对偏差之和，相等再取结束边界向量字典序最小。
// ---------------------------------------------------------------------------

func TestTieBreakLexicographic(t *testing.T) {
	times := uniformTimes(6, 100)
	tp := []float64{100, 100, 100, 100, 100, 100}
	ds := mkDataset(times, tp, constVals(6, 0))
	// 充气可结束于 e=1,2,3（闭区间 [100,300]，终值偏差全为 0）；
	// 缓解都能收尾到样本 5。偏差和全相等，应取向量字典序最小 [1,5]。
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":300},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":400},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "qualified" {
		t.Fatalf("want qualified, got %+v", res.Diagnosis)
	}
	if got := res.EndBoundaryVector; len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("vector = %v, want lex-min [1 5]", got)
	}
}

func TestTieBreakDeviationSum(t *testing.T) {
	times := uniformTimes(6, 100)
	// e=2 终值恰好 100（偏差 0），e=3 终值 99（偏差 1），其余约束一致，应选偏差和更小者。
	tp := []float64{100, 100, 100, 99, 100, 100}
	ds := mkDataset(times, tp, constVals(6, 0))
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":300},
	     "target_final":100,"abs_tolerance":2,"allowed_reverse_cumulative":2,
	     "max_sample_gap_ms":200},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":400},
	     "target_final":100,"abs_tolerance":2,"allowed_reverse_cumulative":2,
	     "max_sample_gap_ms":200}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "qualified" {
		t.Fatalf("want qualified, got %+v", res.Diagnosis)
	}
	if got := res.EndBoundaryVector; got[0] != 2 {
		t.Fatalf("vector = %v, want phase0 end 2 (smaller deviation sum)", got)
	}
	if !approxEq(res.TotalFinalDeviation, 0, 1e-9) {
		t.Fatalf("total deviation = %v, want 0", res.TotalFinalDeviation)
	}
}

// 首阶段即无法开始：诊断从首样本、0 个已完成阶段出发，且不给部分结果。
func TestAlignRejectedAtFirstPhase(t *testing.T) {
	// 采样间隔 100ms，但首阶段最大采样间隔 50ms：首间隔即断口。
	times := uniformTimes(4, 100)
	ds := mkDataset(times, constVals(4, 100), constVals(4, 0))
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":300},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":50},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":300},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "rejected" || len(res.Phases) != 0 {
		t.Fatalf("want rejected with no partial phases, got %+v", res)
	}
	d := res.Diagnosis
	if d.CompletedPhases != 0 || d.NextPhaseIndex != 0 || d.NextStartSample != 0 {
		t.Fatalf("diagnosis must point at phase 0 / sample 0, got %+v", d)
	}
	if d.Violation.Code != ViolSampleGap || d.NextStartCSVRow != 2 || d.NextStartMS != 0 {
		t.Fatalf("want first-gap violation at row 2/ms 0, got %+v", d.Violation)
	}
}

// 诊断路径选结束边界向量字典序最小者：即使更大边界也无法救活下一阶段，仍选小边界。
func TestDiagnosisPicksLexicographicallySmallestPrefix(t *testing.T) {
	times := uniformTimes(8, 100)
	// 充气在 e=1、e=2 均合法；后续目标 0 在数据上不可达，两种前缀都失败，应取 [1]。
	tp := constVals(8, 100)
	ds := mkDataset(times, tp, constVals(8, 0))
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":200},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":900},
	     "target_final":0,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "rejected" {
		t.Fatalf("want rejected, got %s", res.Verdict)
	}
	d := res.Diagnosis
	if d.CompletedPhases != 1 || len(d.EndBoundaryVector) != 1 || d.EndBoundaryVector[0] != 1 {
		t.Fatalf("want lex-min prefix [1], got %+v", d)
	}
	if d.NextStartSample != 1 {
		t.Fatalf("next start must be the lex-min boundary 1, got %d", d.NextStartSample)
	}
}

// ---------------------------------------------------------------------------
// 6) 反向累计量：充气累计压力下降量、缓解累计压力上升量。
// ---------------------------------------------------------------------------

func TestReverseCumulative(t *testing.T) {
	// 充气段 100 -> 110 -> 105 -> 110：累计下降 = 5。
	ds := mkDataset(uniformTimes(4, 100), []float64{100, 110, 105, 110}, constVals(4, 0))
	chg := PhaseSpec{Kind: KindCharging, ControlledColumn: ColTrainPipe,
		Duration: ClosedInterval{Min: 0, Max: 1000}, TargetFinal: 110, AbsTolerance: 0,
		AllowedReverseCum: 5, MaxSampleGapMS: 200}
	ev := evaluate(ds, chg, 0, 3)
	if !approxEq(ev.ReverseCumulative, 5, 1e-9) {
		t.Fatalf("charging reverse cumulative = %v, want 5", ev.ReverseCumulative)
	}
	if !ev.Valid {
		t.Fatalf("cumulative drop exactly at budget must be valid, got %v", ev.Violations)
	}
	chg.AllowedReverseCum = 4.9
	if evaluate(ds, chg, 0, 3).Valid {
		t.Fatal("cumulative drop over budget must be invalid")
	}

	// 缓解段反向 = 累计压力上升：100 -> 90 -> 92 -> 80，累计上升 = 2。
	ds2 := mkDataset(uniformTimes(4, 100), []float64{100, 90, 92, 80}, constVals(4, 0))
	rel := PhaseSpec{Kind: KindRelease, ControlledColumn: ColTrainPipe,
		Duration: ClosedInterval{Min: 0, Max: 1000}, TargetFinal: 80, AbsTolerance: 0,
		AllowedReverseCum: 2, MaxSampleGapMS: 200}
	ev2 := evaluate(ds2, rel, 0, 3)
	if !approxEq(ev2.ReverseCumulative, 2, 1e-9) {
		t.Fatalf("release reverse cumulative = %v, want 2", ev2.ReverseCumulative)
	}
}

// ---------------------------------------------------------------------------
// 7) 回归：保压压力恰从允许带边缘向外偏离，越带时间必须被计入，不得漏报。
// ---------------------------------------------------------------------------

func TestExcursionFromExactBandEdge(t *testing.T) {
	p := PhaseSpec{Kind: KindHolding, ControlledColumn: ColTrainPipe,
		TargetFinal: 100, HoldingBandHalfWidth: 5} // 带 [95,105]

	// 段首恰在上界 105，随后向上偏离到 110：除起点这个零宽界点外整段都带外，
	// 旧实现把段首状态按“界上=带内”处理，会漏掉整段越带。
	outs := segmentExcursions(0, 100, 105, 110, 95, 105)
	if len(outs) != 1 {
		t.Fatalf("edge->above: want one excursion [0,100], got %v", outs)
	}
	if !approxEq(outs[0][0], 0, 1e-9) || !approxEq(outs[0][1], 100, 1e-9) {
		t.Fatalf("edge->above excursion = %v, want [0 100]", outs[0])
	}

	// 段首恰在下界 95，随后向下偏离到 90：同样整段带外。
	outs2 := segmentExcursions(0, 100, 95, 90, 95, 105)
	if len(outs2) != 1 || !approxEq(outs2[0][0], 0, 1e-9) || !approxEq(outs2[0][1], 100, 1e-9) {
		t.Fatalf("edge->below excursion = %v, want [0 100]", outs2)
	}

	// 反向必须仍然不算越带：段首在界点、随后回到带内（105 -> 100）无越带时长。
	if outs3 := segmentExcursions(0, 100, 105, 100, 95, 105); len(outs3) != 0 {
		t.Fatalf("edge->inside must have no excursion, got %v", outs3)
	}
	// 段尾恰在界点、之前整段带外（110 -> 105）：越带 [0,100]，界点本身零宽不扣减。
	outs4 := segmentExcursions(0, 100, 110, 105, 95, 105)
	if len(outs4) != 1 || !approxEq(outs4[0][0], 0, 1e-9) || !approxEq(outs4[0][1], 100, 1e-9) {
		t.Fatalf("above->edge excursion = %v, want [0 100]", outs4)
	}

	// 端到端：样本点恰在界点、之后一直向外，单次越带就是整段 200ms，
	// 预算 100ms 时必须判不合格（旧实现漏报会误判合格）。
	ds := mkDataset([]int64{0, 100, 200}, []float64{105, 108, 110}, constVals(3, 0))
	hp := p
	hp.Duration = ClosedInterval{Min: 200, Max: 200}
	hp.AbsTolerance = 200
	hp.MaxSingleExcursionMS = 100
	hp.MaxTotalExcursionMS = 100
	hp.MaxSampleGapMS = 1000
	if ev := evaluate(ds, hp, 0, 2); ev.Valid {
		t.Fatalf("200ms excursion from exact edge must fail 100ms budget, got valid %+v", ev)
	}
	// 预算恰好 200ms 时合格，且报告的越带时长为精确的 200ms（不放宽也不漏报）。
	hp.MaxSingleExcursionMS = 200
	hp.MaxTotalExcursionMS = 200
	evOK := evaluate(ds, hp, 0, 2)
	if !evOK.Valid {
		t.Fatalf("200ms excursion at exact 200ms budget must be valid, got %v", evOK.Violations)
	}
	if !approxEq(evOK.SingleExcursionMS, 200, 1e-9) || !approxEq(evOK.TotalExcursionMS, 200, 1e-9) {
		t.Fatalf("excursion metrics = %v/%v, want 200/200", evOK.SingleExcursionMS, evOK.TotalExcursionMS)
	}
}

// 跨样本点从界点向外偏离也必须连续计入，不能在界点处被错误闭合。
func TestExcursionFromEdgeAcrossSamples(t *testing.T) {
	p := PhaseSpec{Kind: KindHolding, ControlledColumn: ColTrainPipe,
		TargetFinal: 100, HoldingBandHalfWidth: 5} // 带 [95,105]
	// 110(带外) -> 105(恰在上界) -> 110(再向上)。
	// 中间样本只是越过界点的零宽接触，两侧越带应合并为一次连续 200ms。
	ds := mkDataset([]int64{0, 100, 200}, []float64{110, 105, 110}, constVals(3, 0))
	m := computeHoldMetrics(ds, p, 0, 2)
	if !approxEq(m.single, 200, 1e-9) || !approxEq(m.total, 200, 1e-9) {
		t.Fatalf("across-edge single/total = %v/%v, want merged 200/200", m.single, m.total)
	}
}

// ---------------------------------------------------------------------------
// 8) 回归：终值只超出上限一个极小数值也必须判不合格，系统不得擅自放宽标准。
// ---------------------------------------------------------------------------

func TestFinalValueNoToleranceRelaxation(t *testing.T) {
	rel := PhaseSpec{Kind: KindRelease, ControlledColumn: ColTrainPipe,
		Duration: ClosedInterval{Min: 100, Max: 100}, TargetFinal: 100,
		AllowedReverseCum: 10, MaxSampleGapMS: 200}

	// 容差为 0：终值超出 5e-10 也必须不合格（旧实现的绝对 epsilon=1e-9 会放行）。
	rel0 := rel
	rel0.AbsTolerance = 0
	ds := mkDataset([]int64{0, 100}, []float64{100, 100.0000000005}, constVals(2, 0))
	if ev := evaluate(ds, rel0, 0, 1); ev.Valid {
		t.Fatalf("final value 5e-10 over a zero tolerance must be rejected")
	}
	// 恰好等于目标必须合格。
	ds2 := mkDataset([]int64{0, 100}, []float64{100, 100}, constVals(2, 0))
	if ev2 := evaluate(ds2, rel0, 0, 1); !ev2.Valid {
		t.Fatalf("final value exactly on target must be valid, got %v", ev2.Violations)
	}

	// 有限容差 1：超出 1 仅 5e-10 也必须不合格。旧实现 eps=1e-9*max(1,tol)=1e-9，
	// 会把 5e-10 这个真实超限吞掉而擅自放行合格。
	rel1 := rel
	rel1.AbsTolerance = 1
	ds3 := mkDataset([]int64{0, 100}, []float64{100, 101.0000000005}, constVals(2, 0))
	if ev3 := evaluate(ds3, rel1, 0, 1); ev3.Valid {
		t.Fatalf("final value 5e-10 beyond tolerance must be rejected")
	}
	// 恰好落在容差边界（偏差恰为 1）合格。
	ds4 := mkDataset([]int64{0, 100}, []float64{100, 101}, constVals(2, 0))
	if ev4 := evaluate(ds4, rel1, 0, 1); !ev4.Valid {
		t.Fatalf("final deviation exactly at tolerance boundary must be valid, got %v", ev4.Violations)
	}
}

// ---------------------------------------------------------------------------
// 9) 回归：多个合格切分必须选“终值偏差和最小”者，而不是被字典序抢先。
// ---------------------------------------------------------------------------

func TestAlignPicksMinimumDeviationNotLex(t *testing.T) {
	// 两段：充气在 e=1 与 e=2 均合法，缓解均可收尾到 e=4。
	//   [1,4]: 充气终值偏差 |60-70|=10，缓解终值偏差 |50-50|=0，偏差和 10；
	//   [2,4]: 充气终值偏差 |70-70|=0，缓解终值偏差 0，偏差和 0。
	// 字典序更偏向 [1,4]，但偏差和更小的是 [2,4]，必须选 [2,4]。
	times := uniformTimes(5, 100)
	tp := []float64{50, 60, 70, 60, 50}
	ds := mkDataset(times, tp, constVals(5, 0))
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":200},
	     "target_final":70,"abs_tolerance":20,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":200},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":300},
	     "target_final":50,"abs_tolerance":20,"allowed_reverse_cumulative":20,
	     "max_sample_gap_ms":200}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "qualified" {
		t.Fatalf("want qualified, got %+v", res.Diagnosis)
	}
	if got := res.EndBoundaryVector; len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("must choose min-deviation [2 4] over lex-smaller [1 4], got %v", got)
	}
	if !approxEq(res.TotalFinalDeviation, 0, 1e-12) {
		t.Fatalf("total deviation = %v, want 0", res.TotalFinalDeviation)
	}
}

// 偏差和极小差异也必须被识别（旧实现的相对容差会把它们当并列）。
func TestAlignDistinguishesTinyDeviationDifference(t *testing.T) {
	times := uniformTimes(4, 100)
	// 充气 e=1 终值偏差 1e-10，e=2 终值偏差 0；缓解都到 e=3 且终值偏差 0。
	tp := []float64{100, 100 + 1e-10, 100, 100}
	ds := mkDataset(times, tp, constVals(4, 0))
	spec := mustSpec(t, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":200},
	     "target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":1,
	     "max_sample_gap_ms":200},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":100,"max_ms":200},
	     "target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":1,
	     "max_sample_gap_ms":200}
	  ]}`)
	res := Align(ds, spec)
	if res.Verdict != "qualified" {
		t.Fatalf("want qualified, got %+v", res.Diagnosis)
	}
	// e=2 偏差和 0 更小（虽然 e=1 字典序更小且差仅 1e-10），必须选 [2,3]。
	if got := res.EndBoundaryVector; got[0] != 2 {
		t.Fatalf("tiny deviation difference must be honored, got vector %v", got)
	}
}

// ---------------------------------------------------------------------------
// 10) 回归：同一行同时有时间错误和压力错误时必须一次全部报出。
// ---------------------------------------------------------------------------

func TestCSVReportsTimeAndPressureErrorsOnSameRow(t *testing.T) {
	// 第 3 行：时间非整数（错）+ train_pipe 为 NaN（错），两者都必须出现。
	csvText := "ms,train_pipe,brake_cylinder\n" +
		"0,500,300\n" +
		"100,510,300\n" +
		"150.5,NaN,300\n" +
		"200,530,300\n"
	_, errs := ParseCSV(strings.NewReader(csvText))
	var sawTime, sawPressure bool
	for _, e := range errs {
		if strings.Contains(e.Location, "row 4") && strings.Contains(e.Location, `"ms"`) {
			sawTime = true
		}
		if strings.Contains(e.Location, "row 4") && strings.Contains(e.Location, ColTrainPipe) {
			sawPressure = true
		}
	}
	if !sawTime || !sawPressure {
		t.Fatalf("row 4 must report both time and pressure errors, got %+v (time=%v pressure=%v)",
			errs, sawTime, sawPressure)
	}
}

func TestCSVReportsNonIncreasingTimeAndBadPressureTogether(t *testing.T) {
	// 第 3 行：时间回退（<= 上一已接受时间）+ brake_cylinder 非有限，必须同报。
	csvText := "ms,train_pipe,brake_cylinder\n" +
		"0,500,300\n" +
		"100,510,300\n" +
		"100,520,+Inf\n"
	_, errs := ParseCSV(strings.NewReader(csvText))
	var sawTime, sawPressure bool
	for _, e := range errs {
		if strings.Contains(e.Location, "row 4") && strings.Contains(e.Location, `"ms"`) {
			sawTime = true
		}
		if strings.Contains(e.Location, "row 4") && strings.Contains(e.Location, ColBrakeCylinder) {
			sawPressure = true
		}
	}
	if !sawTime || !sawPressure {
		t.Fatalf("non-increasing time and non-finite pressure on row 4 must both be reported, got %+v", errs)
	}
}
