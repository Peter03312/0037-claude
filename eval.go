package align

import "math"

// Gap 描述一次超过阶段最大采样间隔的断口。
type Gap struct {
	I     int   // 越界采样间隔 [i, i+1]
	Delta int64 // 实际间隔（毫秒）
	Limit int64 // 上限（毫秒）
}

// Eval 是单个候选区段 [Start, End] 的全部检查结果。
// 越带时长来自分段线性插值，交点可能落在非整数毫秒，故用浮点记录。
type Eval struct {
	Start, End        int
	Valid             bool
	DurationMS        int64
	SingleExcursionMS float64
	TotalExcursionMS  float64
	FinalDeviation    float64
	ReverseCumulative float64
	Gaps              []Gap
	Violations        []string // 机器可读的违规代码
}

// 违规代码集合。
const (
	ViolSampleGap    = "sample_gap"
	ViolDuration     = "duration_out_of_range"
	ViolFinalValue   = "final_value_deviation"
	ViolReverseCum   = "reverse_cumulative"
	ViolSingleExcurs = "single_excursion_too_long"
	ViolTotalExcurs  = "total_excursion_too_long"
)

// prefix 保存供 O(1) 区段查询的前缀量。
type prefix struct {
	reverse []float64 // reverse[i+1] 为间隔 [i,i+1] 的反向累计量前缀和
}

func buildPrefix(ds *Dataset, p PhaseSpec) prefix {
	v := ds.Cols[p.ControlledColumn]
	n := ds.NSamp
	rev := make([]float64, n)
	for i := 0; i+1 < n; i++ {
		var d float64
		switch p.Kind {
		case KindCharging:
			if v[i+1] < v[i] { // 充气阶段反向 = 压力下降
				d = v[i] - v[i+1]
			}
		case KindRelease:
			if v[i+1] > v[i] { // 缓解阶段反向 = 压力上升
				d = v[i+1] - v[i]
			}
		}
		rev[i+1] = rev[i] + d
	}
	return prefix{reverse: rev}
}

func countGaps(ds *Dataset, s, e int, limit int64) []Gap {
	var gaps []Gap
	for i := s; i < e; i++ {
		d := ds.Times[i+1] - ds.Times[i]
		if d > limit {
			gaps = append(gaps, Gap{I: i, Delta: d, Limit: limit})
		}
	}
	return gaps
}

// quickEval 在已知保压越带统计量时，对 [s,e] 做一次完整评估。
func quickEval(ds *Dataset, p PhaseSpec, pf prefix, s, e int, single, total float64) Eval {
	v := ds.Cols[p.ControlledColumn]
	ev := Eval{Start: s, End: e, DurationMS: ds.Times[e] - ds.Times[s],
		SingleExcursionMS: single, TotalExcursionMS: total}

	ev.Gaps = countGaps(ds, s, e, p.MaxSampleGapMS)
	if len(ev.Gaps) > 0 {
		ev.Violations = append(ev.Violations, ViolSampleGap)
	}
	if ev.DurationMS < p.Duration.Min || ev.DurationMS > p.Duration.Max {
		ev.Violations = append(ev.Violations, ViolDuration)
	}
	ev.FinalDeviation = math.Abs(v[e] - p.TargetFinal)
	if gt(ev.FinalDeviation, p.AbsTolerance) {
		ev.Violations = append(ev.Violations, ViolFinalValue)
	}
	if p.Kind != KindHolding {
		ev.ReverseCumulative = pf.reverse[e] - pf.reverse[s]
		if gt(ev.ReverseCumulative, p.AllowedReverseCum) {
			ev.Violations = append(ev.Violations, ViolReverseCum)
		}
	} else {
		if gt(single, float64(p.MaxSingleExcursionMS)) {
			ev.Violations = append(ev.Violations, ViolSingleExcurs)
		}
		if gt(total, float64(p.MaxTotalExcursionMS)) {
			ev.Violations = append(ev.Violations, ViolTotalExcurs)
		}
	}
	ev.Valid = len(ev.Violations) == 0
	return ev
}

// gt 是带相对容差的大于比较：落在预算上（闭区间语义）不算超限。
func gt(x, limit float64) bool {
	eps := 1e-9 * math.Max(1, math.Abs(limit))
	return x > limit+eps
}

// evaluate 评估任意区段（诊断路径下的违规区间也用它）。
func evaluate(ds *Dataset, p PhaseSpec, s, e int) Eval {
	pf := buildPrefix(ds, p)
	var single, total float64
	if p.Kind == KindHolding {
		m := computeHoldMetrics(ds, p, s, e)
		single, total = m.single, m.total
	}
	return quickEval(ds, p, pf, s, e, single, total)
}

// ---------- 保压闭带：分段线性插值求越带时长 ----------

// holdState 以固定起点 s 一路增长到当前右端时的增量统计量。
// single/total 只统计已经闭合的越带段；pending 是当前尚未闭合、
// 可能延续到下一个采样段的连续越带（跨样本点的越带不得被拆成两段）。
// 所有时刻均为浮点毫秒（插值交点可能落在两个采样时刻之间）。
type holdState struct {
	single       float64
	total        float64
	pendingStart float64
	pendingEnd   float64
}

type holdStat struct{ single, total float64 }

// snapshot 返回把当前未闭合越带段计入后的最终统计，不修改增量状态。
func (st holdState) snapshot() holdStat {
	single, total := st.single, st.total
	if st.pendingStart >= 0 {
		if l := st.pendingEnd - st.pendingStart; l > single {
			single = l
		}
		total += st.pendingEnd - st.pendingStart
	}
	return holdStat{single: single, total: total}
}

func (st *holdState) closePending() {
	if st.pendingStart < 0 {
		return
	}
	if l := st.pendingEnd - st.pendingStart; l > st.single {
		st.single = l
	}
	st.total += st.pendingEnd - st.pendingStart
	st.pendingStart, st.pendingEnd = -1, 0
}

func computeHoldMetrics(ds *Dataset, p PhaseSpec, s, e int) holdStat {
	st := holdState{pendingStart: -1}
	for i := s; i < e; i++ {
		st = growHold(ds, p, st, i)
	}
	return st.snapshot()
}

// 采样段端点相对闭带的位置：严格带内 / 界点上 / 带外。
const (
	locInside = iota
	locOn
	locOutside
)

func bandLocation(y, lo, hi float64) int {
	switch {
	case y > lo && y < hi:
		return locInside
	case y == lo || y == hi:
		return locOn
	default:
		return locOutside
	}
}

// growHold 追加采样间隔 [i, i+1] 的越带贡献。
// 合并/拆分规则：前段结尾与本段开头在同一样本点，
//   - 两端都带外或界点（之间没有严格带内点）→ 同一次连续越带，合并；
//   - 任一端严格带内 → 夹有带内时间，拆成两次。
func growHold(ds *Dataset, p PhaseSpec, st holdState, i int) holdState {
	v := ds.Cols[p.ControlledColumn]
	lo, hi := p.TargetFinal-p.HoldingBandHalfWidth, p.TargetFinal+p.HoldingBandHalfWidth
	t0, t1 := float64(ds.Times[i]), float64(ds.Times[i+1])
	y0, y1 := v[i], v[i+1]

	// y0 是与上一采样段的公共样本点：该点严格带内时前后越带必须拆成两次。
	if st.pendingStart >= 0 && bandLocation(y0, lo, hi) == locInside {
		st.closePending()
	}

	outs := segmentExcursions(t0, t1, y0, y1, lo, hi)
	for _, in := range outs {
		if st.pendingStart >= 0 && in[0] == st.pendingEnd {
			// 公共点在界点上或带外：同一次连续越带，跨样本点合并。
			if in[1] > st.pendingEnd {
				st.pendingEnd = in[1]
			}
			continue
		}
		// 与 pending 不相接（间隔有严格带内区间）：先闭合旧段再开新段。
		if st.pendingStart >= 0 {
			st.closePending()
		}
		st.pendingStart, st.pendingEnd = in[0], in[1]
	}
	return st
}

// segmentExcursions 返回线性段 [t0,t1]（端点压力 y0,y1）内压力严格位于闭带
// [lo,hi] 之外的时间子区间（浮点毫秒）。闭带边界上的点不算越带。
// 线性函数与带相交最多两次：穿越后的状态由穿越方向唯一确定
// （上穿下界=进入带内，上穿上界=穿出带外；下穿反之），
// 从而正确处理“端点在界内/界上、交点后穿出”等全部情形。
func segmentExcursions(t0, t1, y0, y1, lo, hi float64) [][2]float64 {
	inAt := func(y float64) bool { return y >= lo && y <= hi }

	type pt struct {
		t   float64
		out bool // 越过该点后是否处于带外
	}
	pts := []pt{{t0, !inAt(y0)}}

	if y0 != y1 {
		// 与下界 lo 的交点：下穿后带外，上穿后带内。
		if tL, ok := crossingTime(t0, t1, y0, y1, lo); ok {
			pts = append(pts, pt{tL, y1 < y0})
		}
		// 与上界 hi 的交点：上穿后带外，下穿后带内。
		if tH, ok := crossingTime(t0, t1, y0, y1, hi); ok {
			pts = append(pts, pt{tH, y1 > y0})
		}
	}
	pts = append(pts, pt{t1, !inAt(y1)})

	// 按时刻排序（交点至多两个，简单插入排序）。
	for a := 1; a < len(pts); a++ {
		for b := a; b > 0 && pts[b].t < pts[b-1].t; b-- {
			pts[b], pts[b-1] = pts[b-1], pts[b]
		}
	}
	// 去重：同一时刻两个交点（退化情形）时，穿越净效果为带内。
	uniq := pts[:0]
	for _, q := range pts {
		if n := len(uniq); n > 0 && uniq[n-1].t == q.t {
			uniq[n-1].out = false
			continue
		}
		uniq = append(uniq, q)
	}

	var res [][2]float64
	for k := 0; k+1 < len(uniq); k++ {
		if uniq[k].out {
			res = append(res, [2]float64{uniq[k].t, uniq[k+1].t})
		}
	}
	return res
}

// crossingTime 求线性段压力等于 level 的浮点时刻；与采样时刻重合的交点不返回
// （由端点状态处理），以保证零宽点不被计作越带。
func crossingTime(t0, t1, y0, y1, level float64) (float64, bool) {
	if (level < math.Min(y0, y1)) || (level > math.Max(y0, y1)) {
		return 0, false
	}
	if y0 == y1 {
		return 0, false // 恒值段由端点状态覆盖
	}
	fr := (level - y0) / (y1 - y0)
	if fr <= 0 || fr >= 1 {
		return 0, false
	}
	return t0 + fr*(t1-t0), true
}

// candidateIter 枚举阶段 p 在固定起点 s 下的所有右端 e（e>s），
// 顺序回调每个候选的评估。末阶段强制以最后一个样本结束。
// 保压越带统计随右端增量维护，单起点 O(n) 时间、O(1) 额外内存。
func candidateIter(ds *Dataset, p PhaseSpec, pf prefix, s int, lastPhase bool, fn func(ev Eval)) {
	st := holdState{pendingStart: -1}
	for e := s + 1; e < ds.NSamp; e++ {
		if p.Kind == KindHolding {
			st = growHold(ds, p, st, e-1)
		}
		if lastPhase && e != ds.NSamp-1 {
			continue
		}
		var single, total float64
		if p.Kind == KindHolding {
			m := st.snapshot()
			single, total = m.single, m.total
		}
		ev := quickEval(ds, p, pf, s, e, single, total)
		fn(ev)
	}
}
