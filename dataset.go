package align

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// utf8BOM 是 Windows 导出工具可能加在文件开头的字节序标记。
const utf8BOM = "\ufeff"

// Dataset 是清洗后的整单采样数据。
type Dataset struct {
	Times []int64              // 严格递增的毫秒时间戳
	Cols  map[string][]float64 // 每个受控列一条等长压力序列
	NSamp int
}

// ParseCSV 解析 CSV：第一行为表头，须含 ms 及全部受控列；
// 缺列、时间不递增、非有限数、列数不一致等全部收集为带行列号的错误，整单拒收。
func ParseCSV(r io.Reader) (*Dataset, []ErrorDetail) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // 行数不一致不自动中止，统一收集为带行号错误

	var errs []ErrorDetail
	header, err := cr.Read()
	if err == io.EOF {
		return nil, []ErrorDetail{{Message: "CSV is empty"}}
	}
	if err != nil {
		return nil, []ErrorDetail{{Message: "cannot read CSV header: " + err.Error()}}
	}

	colIndex := map[string]int{}
	for j, h := range header {
		// 兼容带 UTF-8 BOM 的表头（Windows 工具导出常见）：仅剥离首个字段前的 BOM。
		if j == 0 {
			h = strings.TrimPrefix(h, utf8BOM)
		}
		switch h {
		case TimeColumn, ColTrainPipe, ColBrakeCylinder:
			if _, dup := colIndex[h]; dup {
				errs = append(errs, ErrorDetail{Location: "header", Message: fmt.Sprintf("duplicate column %q", h)})
			}
			colIndex[h] = j
		}
	}
	for _, name := range append([]string{TimeColumn}, controlledColumns...) {
		if _, ok := colIndex[name]; !ok {
			errs = append(errs, ErrorDetail{Location: "header", Message: fmt.Sprintf("missing required column %q", name)})
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	// CSV 行号（1-based，表头是第 1 行），便于检修员在原始单据中定位。
	rowNo := func(recordIndex int) int { return recordIndex + 2 }

	times := []int64{}
	values := map[string][]float64{
		ColTrainPipe:     {},
		ColBrakeCylinder: {},
	}

	recordIdx := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, ErrorDetail{
				Location: fmt.Sprintf("row %d", rowNo(recordIdx)),
				Message:  "CSV parse error: " + err.Error(),
			})
			recordIdx++
			if perr, ok := err.(*csv.ParseError); ok && (perr.Err == csv.ErrBareQuote || perr.Err == csv.ErrQuote) {
				// 引号错误后解析器可能失去同步，后续行不可信。
				break
			}
			continue
		}
		rn := rowNo(recordIdx)
		recordIdx++

		if len(rec) != len(header) {
			errs = append(errs, ErrorDetail{
				Location: fmt.Sprintf("row %d", rn),
				Message:  fmt.Sprintf("expected %d fields, got %d", len(header), len(rec)),
			})
			continue
		}

		msRaw := rec[colIndex[TimeColumn]]
		msf, perr := strconv.ParseFloat(msRaw, 64)
		// 上限 1e15ms（约三万年）足以覆盖任何检修记录，同时杜绝 float->int64 溢出。
		if perr != nil || !finite(msf) || msf != math.Trunc(msf) || msf < 0 || msf > 1e15 {
			errs = append(errs, ErrorDetail{
				Location: fmt.Sprintf("row %d, column %q", rn, TimeColumn),
				Message:  "must be a non-negative integer number of milliseconds (<= 1e15)",
			})
			continue
		}
		ms := int64(msf)
		if len(times) > 0 && ms <= times[len(times)-1] {
			errs = append(errs, ErrorDetail{
				Location: fmt.Sprintf("row %d, column %q", rn, TimeColumn),
				Message:  fmt.Sprintf("time %d is not strictly greater than previous accepted time %d", ms, times[len(times)-1]),
			})
			continue
		}

		parsed := map[string]float64{}
		rowValid := true
		for _, name := range controlledColumns {
			v, perr := strconv.ParseFloat(rec[colIndex[name]], 64)
			if perr != nil {
				errs = append(errs, ErrorDetail{
					Location: fmt.Sprintf("row %d, column %q", rn, name),
					Message:  "cannot parse number: " + perr.Error(),
				})
				rowValid = false
				continue
			}
			if !finite(v) {
				errs = append(errs, ErrorDetail{
					Location: fmt.Sprintf("row %d, column %q", rn, name),
					Message:  "must be a finite number",
				})
				rowValid = false
				continue
			}
			parsed[name] = v
		}
		if !rowValid {
			continue
		}

		times = append(times, ms)
		for _, name := range controlledColumns {
			values[name] = append(values[name], parsed[name])
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}
	if len(times) < 2 {
		return nil, []ErrorDetail{{Message: fmt.Sprintf("need at least 2 valid data rows, got %d (a non-degenerate phase needs a start and an end sample)", len(times))}}
	}

	return &Dataset{Times: times, Cols: values, NSamp: len(times)}, nil
}
