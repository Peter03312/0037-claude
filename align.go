package align

import (
	"fmt"
	"math"
)

// BoundaryReport 是合格方案中单个阶段的可追溯验算明细。
type BoundaryReport struct {
	PhaseIndex           int      `json:"phase_index"`
	Kind                 string   `json:"kind"`
	ControlledColumn     string   `json:"controlled_column"`
	StartSample          int      `json:"start_sample"`
	EndSample            int      `json:"end_sample"`
	StartCSVRow          int      `json:"start_csv_row"`
	EndCSVRow            int      `json:"end_csv_row"`
	StartMS              int64    `json:"start_ms"`
	EndMS                int64    `json:"end_ms"`
	DurationMS           int64    `json:"duration_ms"`
	DurationRangeMS      [2]int64 `json:"duration_range_ms"`
	FinalValue           float64  `json:"final_value"`
	TargetFinal          float64  `json:"target_final"`
	FinalDeviation       float64  `json:"final_deviation"`
	AbsTolerance         float64  `json:"abs_tolerance"`
	ReverseCumulative    float64  `json:"reverse_cumulative,omitempty"`
	AllowedReverseCum    float64  `json:"allowed_reverse_cumulative,omitempty"`
	SingleExcursionMS    float64  `json:"single_out_of_band_ms,omitempty"`
	TotalExcursionMS     float64  `json:"total_out_of_band_ms,omitempty"`
	HoldingBandHalfWidth float64  `json:"holding_band_half_width,omitempty"`
	MaxSingleExcursionMS int64    `json:"max_single_out_of_band_ms,omitempty"`
	MaxTotalExcursionMS  int64    `json:"max_total_out_of_band_ms,omitempty"`
	MaxSampleGapMS       int64    `json:"max_sample_gap_ms"`
}

// IntervalRef 以样本序号、CSV 行号、毫秒时间三重坐标引用区间，供追溯。
type IntervalRef struct {
	StartSample int   `json:"start_sample"`
	EndSample   int   `json:"end_sample"`
	StartCSVRow int   `json:"start_csv_row"`
	EndCSVRow   int   `json:"end_csv_row"`
	StartMS     int64 `json:"start_ms"`
	EndMS       int64 `json:"end_ms"`
}

// GapReport 是断口的可追溯描述。
type GapReport struct {
	FromSample int   `json:"from_sample"`
	ToSample   int   `json:"to_sample"`
	FromCSVRow int   `json:"from_csv_row"`
	ToCSVRow   int   `json:"to_csv_row"`
	ActualMS   int64 `json:"actual_ms"`
	LimitMS    int64 `json:"limit_ms"`
}

// Violation 描述违规区间上的首要违规及实测/限值。
type Violation struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Interval   IntervalRef `json:"interval"`
	Actual     float64     `json:"actual,omitempty"`
	Allowed    float64     `json:"allowed,omitempty"`
	MinAllowed float64     `json:"min_allowed,omitempty"`
	MaxAllowed float64     `json:"max_allowed,omitempty"`
	Target     float64     `json:"target,omitempty"`
	Gaps       []GapReport `json:"gaps,omitempty"`
	Units      string      `json:"units,omitempty"` // "ms" 或 "pressure"，标明 actual/allowed 量纲
}

// Diagnosis 是无完整切分时的诊断路径信息；明确标记 rejected，不给部分合格。
type Diagnosis struct {
	CompletedPhases   int       `json:"completed_phases"`
	EndBoundaryVector []int     `json:"end_boundary_vector"`
	NextPhaseIndex    int       `json:"next_phase_index"`
	NextStartSample   int       `json:"next_start_sample"`
	NextStartCSVRow   int       `json:"next_start_csv_row"`
	NextStartMS       int64     `json:"next_start_ms"`
	Violation         Violation `json:"violation"`
}

// Result 是整周期对齐结论。
type Result struct {
	Verdict             string           `json:"verdict"` // "qualified" | "rejected"
	SampleCount         int              `json:"sample_count"`
	PhaseCount          int              `json:"phase_count"`
	Phases              []BoundaryReport `json:"phases,omitempty"`
	EndBoundaryVector   []int            `json:"end_boundary_vector,omitempty"`
	TotalFinalDeviation float64          `json:"total_final_deviation,omitempty"`
	Diagnosis           *Diagnosis       `json:"diagnosis,omitempty"`
}

// scoreNode 是按“终值偏差之和最优”保留的 DP 状态；
// dev 相同的并列状态之间保留结束边界向量字典序最小者。
// 向量不直接存储，而由父链在需要时逐层回溯，避免每候选 O(n) 复制。
type scoreNode struct {
	end    int
	depth  int // 已覆盖阶段数（root 为 0）
	dev    float64
	parent *scoreNode
}

// lexNode 按“结束边界向量字典序最小”保留，仅用于无完整切分时选取诊断路径。
type lexNode struct {
	end    int
	depth  int
	parent *lexNode
}

var scoreRoot = &scoreNode{end: -1}
var lexRoot = &lexNode{end: -1}

// chainVector 沿父链还原结束边界向量（阶段数少，仅在择优/输出时调用）。
func scoreVector(n *scoreNode) []int {
	v := make([]int, n.depth)
	for i := n.depth - 1; i >= 0; i-- {
		v[i] = n.end
		n = n.parent
	}
	return v
}

func lexVector(n *lexNode) []int {
	if n == lexRoot || n == nil {
		return nil
	}
	v := make([]int, n.depth)
	for i := n.depth - 1; i >= 0; i-- {
		v[i] = n.end
		n = n.parent
	}
	return v
}

// lexCompareScore 在同层节点间比较结束边界向量字典序。
func lexCompareScore(a, b *scoreNode) int {
	va := scoreVector(a)
	vb := scoreVector(b)
	return compareIntVec(va, vb)
}

func lexCompareLex(a, b *lexNode) int {
	return compareIntVec(lexVector(a), lexVector(b))
}

func compareIntVec(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// scoreBetter 报告候选 (dev,node) 是否严格优于同右端的当前状态：
// 偏差和更小；偏差和在容差内相等时结束边界向量字典序更小。
func scoreBetter(dev float64, node *scoreNode, cur *scoreNode) bool {
	eps := 1e-9 * math.Max(1, math.Abs(cur.dev))
	if math.Abs(dev-cur.dev) > eps {
		return dev < cur.dev
	}
	return lexCompareScore(node, cur) < 0
}

// Align 对整单数据做全周期对齐：枚举每个阶段的全部合法区段后用动态规划拼接，
// 相邻阶段共享一个边界样本、首阶段始于首样本、末阶段止于末样本。
// 绝不采用“逐段取首个局部匹配”的贪心策略。
func Align(ds *Dataset, spec *Spec) *Result {
	n := ds.NSamp
	m := len(spec.Phases)
	prefixes := make([]prefix, m)
	for k, p := range spec.Phases {
		prefixes[k] = buildPrefix(ds, p)
	}

	var scoreEnds []*scoreNode
	var lexEnds []*lexNode
	failedAt := -1

	for k := 0; k < m; k++ {
		p := spec.Phases[k]
		lastPhase := k == m-1
		nextScore := make([]*scoreNode, n)
		nextLex := make([]*lexNode, n)

		type source struct {
			s       int
			scoreAt *scoreNode
			lexAt   *lexNode
		}
		var sources []source
		if k == 0 {
			// 首阶段必须从 CSV 首行开始。
			sources = []source{{s: 0, scoreAt: scoreRoot, lexAt: lexRoot}}
		} else {
			for s := 0; s < n; s++ {
				if scoreEnds[s] != nil || lexEnds[s] != nil {
					sources = append(sources, source{s: s, scoreAt: scoreEnds[s], lexAt: lexEnds[s]})
				}
			}
		}

		for _, src := range sources {
			s := src.s
			candidateIter(ds, p, prefixes[k], s, lastPhase, func(ev Eval) {
				if !ev.Valid {
					return
				}
				// 最优偏差路径（并列时字典序最小）。
				if src.scoreAt != nil {
					dev := src.scoreAt.dev + ev.FinalDeviation
					cand := &scoreNode{end: ev.End, depth: src.scoreAt.depth + 1, dev: dev, parent: src.scoreAt}
					cur := nextScore[ev.End]
					if cur == nil || scoreBetter(dev, cand, cur) {
						nextScore[ev.End] = cand
					}
				}
				// 字典序路径（覆盖所有可达候选）。
				if src.lexAt != nil {
					cand := &lexNode{end: ev.End, depth: src.lexAt.depth + 1, parent: src.lexAt}
					cur := nextLex[ev.End]
					if cur == nil || lexCompareLex(cand, cur) < 0 {
						nextLex[ev.End] = cand
					}
				}
			})
		}

		reachable := false
		for _, q := range nextLex {
			if q != nil {
				reachable = true
				break
			}
		}
		if !reachable {
			failedAt = k
			break
		}
		scoreEnds, lexEnds = nextScore, nextLex
	}

	res := &Result{SampleCount: n, PhaseCount: m}

	if failedAt == -1 {
		// 全周期对齐成功：偏差和最小，并列时结束边界向量字典序最小。
		var best *scoreNode
		for _, q := range scoreEnds {
			if q == nil {
				continue
			}
			if best == nil || scoreBetter(q.dev, q, best) {
				best = q
			}
		}
		if best == nil {
			// 不可达：failedAt==-1 意味着每层都有可达候选，末阶段又强制末样本。
			failedAt = m - 1
		} else {
			vec := scoreVector(best)
			res.Verdict = "qualified"
			res.EndBoundaryVector = vec
			res.TotalFinalDeviation = best.dev
			res.Phases = buildBoundaryReports(ds, spec, vec)
			return res
		}
	}

	// 无完整切分：rejected，按已完成阶段最多、结束边界向量最小给诊断路径。
	res.Verdict = "rejected"
	k := failedAt
	var diag *Diagnosis
	if k == 0 {
		diag = buildDiagnosis(ds, spec, k, 0, nil)
	} else {
		var bestLex *lexNode
		for _, q := range lexEnds {
			if q == nil {
				continue
			}
			if bestLex == nil || compareIntVec(lexVector(q), lexVector(bestLex)) < 0 {
				bestLex = q
			}
		}
		vec := lexVector(bestLex)
		s := vec[len(vec)-1]
		diag = buildDiagnosis(ds, spec, k, s, vec)
	}
	res.Diagnosis = diag
	return res
}

func buildBoundaryReports(ds *Dataset, spec *Spec, ends []int) []BoundaryReport {
	out := make([]BoundaryReport, 0, len(spec.Phases))
	start := 0
	for k, p := range spec.Phases {
		end := ends[k]
		ev := evaluate(ds, p, start, end)
		br := BoundaryReport{
			PhaseIndex:       k,
			Kind:             p.Kind,
			ControlledColumn: p.ControlledColumn,
			StartSample:      start,
			EndSample:        end,
			StartCSVRow:      start + 2,
			EndCSVRow:        end + 2,
			StartMS:          ds.Times[start],
			EndMS:            ds.Times[end],
			DurationMS:       ev.DurationMS,
			DurationRangeMS:  [2]int64{p.Duration.Min, p.Duration.Max},
			FinalValue:       ds.Cols[p.ControlledColumn][end],
			TargetFinal:      p.TargetFinal,
			FinalDeviation:   ev.FinalDeviation,
			AbsTolerance:     p.AbsTolerance,
			MaxSampleGapMS:   p.MaxSampleGapMS,
		}
		if p.Kind == KindHolding {
			br.SingleExcursionMS = ev.SingleExcursionMS
			br.TotalExcursionMS = ev.TotalExcursionMS
			br.HoldingBandHalfWidth = p.HoldingBandHalfWidth
			br.MaxSingleExcursionMS = p.MaxSingleExcursionMS
			br.MaxTotalExcursionMS = p.MaxTotalExcursionMS
		} else {
			br.ReverseCumulative = ev.ReverseCumulative
			br.AllowedReverseCum = p.AllowedReverseCum
		}
		out = append(out, br)
		start = end // 相邻阶段共享一个边界样本
	}
	return out
}

// violationPriority 规定同一区间上多种违规时首要违规的报告顺序。
var violationPriority = []string{
	ViolSampleGap,
	ViolDuration,
	ViolFinalValue,
	ViolReverseCum,
	ViolSingleExcurs,
	ViolTotalExcurs,
}

func buildDiagnosis(ds *Dataset, spec *Spec, k, s int, vector []int) *Diagnosis {
	p := spec.Phases[k]
	lastPhase := k == len(spec.Phases)-1

	diag := &Diagnosis{
		CompletedPhases:   k,
		EndBoundaryVector: append([]int{}, vector...),
		NextPhaseIndex:    k,
		NextStartSample:   s,
		NextStartCSVRow:   s + 2,
		NextStartMS:       ds.Times[s],
	}

	switch {
	case s >= ds.NSamp-1:
		// 前一阶段把边界放在了最后一个样本，下一阶段连一个采样间隔都没有。
		ev := Eval{Start: s, End: s, DurationMS: 0, Violations: []string{ViolDuration}}
		diag.Violation = buildViolation(ds, p, k, ev)
		return diag
	case countGaps(ds, s, s+1, p.MaxSampleGapMS) != nil:
		// 首间隔即断口：断口使任何包含该间隔的候选无效，且违规区间序最小。
		diag.Violation = buildViolation(ds, p, k, evaluate(ds, p, s, s+1))
		return diag
	}

	// 从该起点枚举右端，取边界序最小的非法候选。
	// DP 不变量：failedAt==k 意味着从该起点不存在任何合法候选，
	// 因此循环必然命中一个非法候选并返回。
	for e := s + 1; e < ds.NSamp; e++ {
		if lastPhase && e != ds.NSamp-1 {
			continue
		}
		ev := evaluate(ds, p, s, e)
		if !ev.Valid {
			diag.Violation = buildViolation(ds, p, k, ev)
			return diag
		}
	}

	// 不可达：failedAt==k 与“从 lex 最小起点存在合法候选”矛盾（见函数上方 DP 不变量）。
	panic("align: reachable boundary chosen for diagnosis unexpectedly has a valid candidate")
}

func intervalRef(ds *Dataset, start, end int) IntervalRef {
	return IntervalRef{
		StartSample: start,
		EndSample:   end,
		StartCSVRow: start + 2,
		EndCSVRow:   end + 2,
		StartMS:     ds.Times[start],
		EndMS:       ds.Times[end],
	}
}

func buildViolation(ds *Dataset, p PhaseSpec, phaseIndex int, ev Eval) Violation {
	ref := IntervalRef{
		StartSample: ev.Start,
		EndSample:   ev.End,
		StartCSVRow: ev.Start + 2,
		EndCSVRow:   ev.End + 2,
		StartMS:     ds.Times[ev.Start],
		EndMS:       ds.Times[ev.End],
	}
	has := func(code string) bool {
		for _, c := range ev.Violations {
			if c == code {
				return true
			}
		}
		return false
	}
	code := ""
	for _, c := range violationPriority {
		if has(c) {
			code = c
			break
		}
	}
	v := Violation{Code: code, Interval: ref}
	finalValue := ds.Cols[p.ControlledColumn][ev.End]
	switch code {
	case "":
		// 兜底：从该起点存在合法候选，但都无法支撑后续整周期。
		v.Code = ViolDuration
		v.Message = "no candidate beginning at this boundary can complete the remaining whole cycle"
		v.Units = "ms"
	case ViolSampleGap:
		for _, g := range ev.Gaps {
			v.Gaps = append(v.Gaps, GapReport{
				FromSample: g.I,
				ToSample:   g.I + 1,
				FromCSVRow: g.I + 2,
				ToCSVRow:   g.I + 3,
				ActualMS:   g.Delta,
				LimitMS:    g.Limit,
			})
		}
		v.Message = fmt.Sprintf("phase %d: adjacent sample gap exceeds max_sample_gap_ms", phaseIndex)
		v.Units = "ms"
		v.Actual = float64(v.Gaps[0].ActualMS)
		v.Allowed = float64(v.Gaps[0].LimitMS)
	case ViolDuration:
		v.Message = "closed interval duration is outside [min_ms, max_ms]"
		v.Units = "ms"
		v.Actual = float64(ev.DurationMS)
		v.MinAllowed = float64(p.Duration.Min)
		v.MaxAllowed = float64(p.Duration.Max)
	case ViolFinalValue:
		v.Message = "final controlled pressure deviates from target_final beyond abs_tolerance"
		v.Units = "pressure"
		v.Actual = finalValue
		v.Target = p.TargetFinal
		v.Allowed = p.AbsTolerance
	case ViolReverseCum:
		v.Message = "reverse cumulative pressure exceeds budget (charging: cumulative drop; release: cumulative rise)"
		v.Units = "pressure"
		v.Actual = ev.ReverseCumulative
		v.Allowed = p.AllowedReverseCum
	case ViolSingleExcurs:
		v.Message = "a single continuous out-of-band duration exceeds budget (piecewise-linear intersection)"
		v.Units = "ms"
		v.Actual = ev.SingleExcursionMS
		v.Allowed = float64(p.MaxSingleExcursionMS)
	case ViolTotalExcurs:
		v.Message = "total out-of-band duration exceeds budget"
		v.Units = "ms"
		v.Actual = ev.TotalExcursionMS
		v.Allowed = float64(p.MaxTotalExcursionMS)
	}
	return v
}
