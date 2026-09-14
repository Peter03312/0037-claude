package align

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// 受控压力列：列车管、制动缸压力。列名即 CSV 表头，不硬编码阶段与列的对应关系，
// 每阶段在 JSON 中显式指定本阶段受哪一列控制。
const (
	ColTrainPipe     = "train_pipe"
	ColBrakeCylinder = "brake_cylinder"
	TimeColumn       = "ms"
)

var controlledColumns = []string{ColTrainPipe, ColBrakeCylinder}

func isControlledColumn(c string) bool {
	return c == ColTrainPipe || c == ColBrakeCylinder
}

const (
	KindCharging = "charging"
	KindHolding  = "holding"
	KindRelease  = "release"
)

// ClosedInterval 是闭区间时长 [Min, Max]，单位毫秒。
type ClosedInterval struct {
	Min int64 `json:"min_ms"`
	Max int64 `json:"max_ms"`
}

// PhaseSpec 是单个阶段（充气 / 保压 / 缓解）的全部受控参数。
type PhaseSpec struct {
	Kind                 string         `json:"kind"`
	ControlledColumn     string         `json:"controlled_column"`
	Duration             ClosedInterval `json:"duration_ms"`
	TargetFinal          float64        `json:"target_final"`
	AbsTolerance         float64        `json:"abs_tolerance"`
	AllowedReverseCum    float64        `json:"allowed_reverse_cumulative"`
	HoldingBandHalfWidth float64        `json:"holding_band_half_width"`
	MaxSingleExcursionMS int64          `json:"max_single_out_of_band_ms"`
	MaxTotalExcursionMS  int64          `json:"max_total_out_of_band_ms"`
	MaxSampleGapMS       int64          `json:"max_sample_gap_ms"`
}

// Spec 是有序的阶段定义，阶段顺序即全周期顺序。
type Spec struct {
	Phases []PhaseSpec `json:"phases"`
}

// ErrorDetail 定位到具体行/字段的输入错误，整单一次性报出。
type ErrorDetail struct {
	Location string `json:"location,omitempty"`
	Message  string `json:"message"`
}

func phaseLoc(i int, field string) string {
	return fmt.Sprintf("phases[%d].%s", i, field)
}

// rawPhase 用指针区分“未提供”与“提供为零值”。
type rawPhase struct {
	Kind                 *string      `json:"kind"`
	ControlledColumn     *string      `json:"controlled_column"`
	Duration             *rawInterval `json:"duration_ms"`
	TargetFinal          *float64     `json:"target_final"`
	AbsTolerance         *float64     `json:"abs_tolerance"`
	AllowedReverseCum    *float64     `json:"allowed_reverse_cumulative"`
	HoldingBandHalfWidth *float64     `json:"holding_band_half_width"`
	MaxSingleExcursionMS *int64       `json:"max_single_out_of_band_ms"`
	MaxTotalExcursionMS  *int64       `json:"max_total_out_of_band_ms"`
	MaxSampleGapMS       *int64       `json:"max_sample_gap_ms"`
}

type rawInterval struct {
	Min *int64 `json:"min_ms"`
	Max *int64 `json:"max_ms"`
}

type rawSpec struct {
	Phases []rawPhase `json:"phases"`
}

// ParseSpec 解析并完整校验 JSON 规格；错误全部收集后返回，绝不带错继续对齐。
func ParseSpec(data []byte) (*Spec, []ErrorDetail) {
	var raw rawSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, []ErrorDetail{{Message: "invalid JSON: " + err.Error()}}
	}
	if dec.More() {
		return nil, []ErrorDetail{{Message: "invalid JSON: multiple top-level values"}}
	}

	var errs []ErrorDetail
	if len(raw.Phases) == 0 {
		errs = append(errs, ErrorDetail{Location: "phases", Message: "at least one phase is required"})
		return nil, errs
	}

	spec := &Spec{Phases: make([]PhaseSpec, len(raw.Phases))}
	for i, rp := range raw.Phases {
		p := PhaseSpec{}

		if rp.Kind == nil {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "kind"), Message: "is required"})
		} else {
			switch *rp.Kind {
			case KindCharging, KindHolding, KindRelease:
				p.Kind = *rp.Kind
			default:
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "kind"), Message: fmt.Sprintf("must be one of %q, %q, %q", KindCharging, KindHolding, KindRelease)})
			}
		}

		if rp.ControlledColumn == nil {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "controlled_column"), Message: "is required"})
		} else if !isControlledColumn(*rp.ControlledColumn) {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "controlled_column"), Message: fmt.Sprintf("must be one of %q, %q", ColTrainPipe, ColBrakeCylinder)})
		} else {
			p.ControlledColumn = *rp.ControlledColumn
		}

		if rp.Duration == nil {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "duration_ms"), Message: "is required"})
		} else {
			if rp.Duration.Min == nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "duration_ms.min_ms"), Message: "is required"})
			}
			if rp.Duration.Max == nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "duration_ms.max_ms"), Message: "is required"})
			}
			if rp.Duration.Min != nil && rp.Duration.Max != nil {
				p.Duration.Min = *rp.Duration.Min
				p.Duration.Max = *rp.Duration.Max
				if p.Duration.Min < 0 {
					errs = append(errs, ErrorDetail{Location: phaseLoc(i, "duration_ms.min_ms"), Message: "must be >= 0"})
				}
				if p.Duration.Max < p.Duration.Min {
					errs = append(errs, ErrorDetail{Location: phaseLoc(i, "duration_ms"), Message: "max_ms must be >= min_ms"})
				}
			}
		}

		if rp.TargetFinal == nil {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "target_final"), Message: "is required"})
		} else {
			p.TargetFinal = *rp.TargetFinal
			if !finite(p.TargetFinal) {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "target_final"), Message: "must be a finite number"})
			}
		}

		if rp.AbsTolerance == nil {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "abs_tolerance"), Message: "is required"})
		} else {
			p.AbsTolerance = *rp.AbsTolerance
			if p.AbsTolerance < 0 || !finite(p.AbsTolerance) {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "abs_tolerance"), Message: "must be a finite number >= 0"})
			}
		}

		if rp.AllowedReverseCum == nil {
			if p.Kind != KindHolding && p.Kind != "" {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "allowed_reverse_cumulative"), Message: "is required for a charging/release phase"})
			}
		} else if p.Kind == KindHolding {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "allowed_reverse_cumulative"), Message: "only allowed for a charging/release phase"})
		} else {
			p.AllowedReverseCum = *rp.AllowedReverseCum
			if p.AllowedReverseCum < 0 || !finite(p.AllowedReverseCum) {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "allowed_reverse_cumulative"), Message: "must be a finite number >= 0"})
			}
		}

		if rp.MaxSampleGapMS == nil {
			errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_sample_gap_ms"), Message: "is required"})
		} else {
			p.MaxSampleGapMS = *rp.MaxSampleGapMS
			if p.MaxSampleGapMS < 0 {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_sample_gap_ms"), Message: "must be >= 0"})
			}
		}

		if p.Kind == KindHolding {
			if rp.HoldingBandHalfWidth == nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "holding_band_half_width"), Message: "is required for a holding phase"})
			} else {
				p.HoldingBandHalfWidth = *rp.HoldingBandHalfWidth
				if p.HoldingBandHalfWidth <= 0 || !finite(p.HoldingBandHalfWidth) {
					errs = append(errs, ErrorDetail{Location: phaseLoc(i, "holding_band_half_width"), Message: "must be a finite number > 0"})
				}
			}
			if rp.MaxSingleExcursionMS == nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_single_out_of_band_ms"), Message: "is required for a holding phase"})
			} else if *rp.MaxSingleExcursionMS < 0 {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_single_out_of_band_ms"), Message: "must be >= 0"})
			} else {
				p.MaxSingleExcursionMS = *rp.MaxSingleExcursionMS
			}
			if rp.MaxTotalExcursionMS == nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_total_out_of_band_ms"), Message: "is required for a holding phase"})
			} else if *rp.MaxTotalExcursionMS < 0 {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_total_out_of_band_ms"), Message: "must be >= 0"})
			} else {
				p.MaxTotalExcursionMS = *rp.MaxTotalExcursionMS
			}
		} else {
			if rp.HoldingBandHalfWidth != nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "holding_band_half_width"), Message: "only allowed for a holding phase"})
			}
			if rp.MaxSingleExcursionMS != nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_single_out_of_band_ms"), Message: "only allowed for a holding phase"})
			}
			if rp.MaxTotalExcursionMS != nil {
				errs = append(errs, ErrorDetail{Location: phaseLoc(i, "max_total_out_of_band_ms"), Message: "only allowed for a holding phase"})
			}
		}

		spec.Phases[i] = p
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return spec, nil
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
