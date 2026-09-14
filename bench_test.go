package align

import "testing"

// 最坏规模冒烟：大量样本、单阶段必须覆盖全周期，候选枚举 O(n)。
func BenchmarkAlignSinglePhase(b *testing.B) {
	const n = 50000
	times := make([]int64, n)
	tp := make([]float64, n)
	for i := range times {
		times[i] = int64(i)
		tp[i] = 100
	}
	ds := mkDataset(times, tp, make([]float64, n))
	spec := mustSpec(b, `{
	  "phases": [
	    {"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":100000000},
	     "target_final":100,"abs_tolerance":0,
	     "holding_band_half_width":1,"max_single_out_of_band_ms":0,"max_total_out_of_band_ms":0,
	     "max_sample_gap_ms":100000000}
	]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := Align(ds, spec)
		if r.Verdict != "qualified" {
			b.Fatal("want qualified")
		}
	}
}

// 多阶段多候选：两层合法右端稠密，检验 DP 组合规模的可控性。
func BenchmarkAlignTwoDensePhases(b *testing.B) {
	const n = 5000
	times := make([]int64, n)
	tp := make([]float64, n)
	for i := range times {
		times[i] = int64(i)
		tp[i] = 100
	}
	ds := mkDataset(times, tp, make([]float64, n))
	spec := mustSpec(b, `{
	  "phases": [
	    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":1,"max_ms":100000000},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":100000000},
	    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":1,"max_ms":100000000},
	     "target_final":100,"abs_tolerance":0,"allowed_reverse_cumulative":0,
	     "max_sample_gap_ms":100000000}
	]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := Align(ds, spec)
		if r.Verdict != "qualified" {
			b.Fatal("want qualified")
		}
	}
}
