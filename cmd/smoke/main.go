// Command smoke 对运行中的验算 API 做端到端 HTTP 冒烟：
//   - GET /healthz 存活检查；
//   - POST /verify 三阶段（充气/保压/缓解）整单，断言 qualified、共享边界与插值越带时长；
//   - POST /verify 缺列坏单，断言 400 且带行列错误。
//
// 期望值（边界、越带时长）由服务端算法计算，冒烟只做独立断言，不参与对齐。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	base := flag.String("url", envOr("SMOKE_URL", "http://localhost:8080"), "API base URL")
	flag.Parse()

	client := &http.Client{Timeout: 15 * time.Second}
	failures := 0
	check := func(name string, cond bool, detail string) {
		if cond {
			fmt.Printf("PASS %s\n", name)
			return
		}
		fmt.Printf("FAIL %s: %s\n", name, detail)
		failures++
	}

	// 1) 存活探针。
	resp, err := client.Get(*base + "/healthz")
	if err != nil {
		fatalf("healthz request failed: %v", err)
	}
	resp.Body.Close()
	check("healthz returns 200", resp.StatusCode == 200, fmt.Sprintf("status=%d", resp.StatusCode))

	// 2) 合格整单（间隔 100ms，受控列 train_pipe；brake_cylinder 为恒值陪衬列）。
	//
	//	idx:  0   1   2    3    4   5   6   7   8
	//	v:    50  80  110  100  100  90  70  50  40
	//
	// 充气 [0,2] 200ms；保压 [2,4] 200ms，样本 2 在带 [95,105] 上方（110），
	// 样本 3 回到 100，上界交点在 50ms，单次/累计越带恰为插值求得的 50ms；
	// 缓解 [4,8] 400ms，压力单调下降，终值 40。
	times := []int64{0, 100, 200, 300, 400, 500, 600, 700, 800}
	trainPipe := []float64{50, 80, 110, 100, 100, 90, 70, 50, 40}
	csvGood := buildCSV(times, trainPipe, constCol(300, len(times)))
	specGood := `{
  "phases": [
    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":200},
     "target_final":110,"abs_tolerance":1,"allowed_reverse_cumulative":0,
     "max_sample_gap_ms":200},
    {"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":200,"max_ms":200},
     "target_final":100,"abs_tolerance":5,
     "holding_band_half_width":5,"max_single_out_of_band_ms":50,"max_total_out_of_band_ms":50,
     "max_sample_gap_ms":200},
    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":400,"max_ms":400},
     "target_final":40,"abs_tolerance":1,"allowed_reverse_cumulative":0,
     "max_sample_gap_ms":200}
  ]
}`
	body, status, err := postVerify(client, *base, csvGood, specGood)
	if err != nil {
		fatalf("verify request failed: %v", err)
	}
	check("qualified order returns 200", status == 200, fmt.Sprintf("status=%d body=%s", status, truncate(body)))

	var good map[string]any
	if err := json.Unmarshal(body, &good); err != nil {
		fatalf("cannot decode qualified response: %v", err)
	}
	check("verdict is qualified", good["verdict"] == "qualified", fmt.Sprintf("verdict=%v", good["verdict"]))
	check("end boundary vector is [2,4,8] (shared boundaries)",
		intVec(good["end_boundary_vector"]) == "2,4,8",
		fmt.Sprintf("vector=%v", good["end_boundary_vector"]))

	phases, _ := good["phases"].([]any)
	if len(phases) == 3 {
		hold := phases[1].(map[string]any)
		check("holding interpolated single excursion is 50 ms",
			floatVal(hold["single_out_of_band_ms"]) == 50,
			fmt.Sprintf("single=%v", hold["single_out_of_band_ms"]))
		check("holding interpolated total excursion is 50 ms",
			floatVal(hold["total_out_of_band_ms"]) == 50,
			fmt.Sprintf("total=%v", hold["total_out_of_band_ms"]))
		// 共享边界：阶段 1 起点 == 阶段 0 终点；阶段 2 起点 == 阶段 1 终点。
		ch := phases[0].(map[string]any)
		rel := phases[2].(map[string]any)
		shared := int64Val(ch["end_sample"]) == int64Val(hold["start_sample"]) &&
			int64Val(hold["end_sample"]) == int64Val(rel["start_sample"])
		check("adjacent phases share exactly one boundary sample", shared,
			fmt.Sprintf("ends=%v/%v/%v", ch["end_sample"], hold["end_sample"], rel["end_sample"]))
		check("first phase starts on first CSV data row",
			int64Val(ch["start_sample"]) == 0 && int64Val(ch["start_csv_row"]) == 2,
			"first phase does not start at row 2")
		check("last phase ends on last CSV data row",
			int64Val(rel["end_sample"]) == 8 && int64Val(rel["end_csv_row"]) == 10,
			"last phase does not end at the last row")
	} else {
		check("response contains 3 phase reports", false, fmt.Sprintf("phases=%v", phases))
	}

	// 3) 缺列坏单：整单 400，错误带行列定位。
	bad := "ms,train_pipe\n0,500\n40,520\n"
	body, status, err = postVerify(client, *base, bad, specGood)
	if err != nil {
		fatalf("bad-order request failed: %v", err)
	}
	check("missing column order returns 400", status == 400, fmt.Sprintf("status=%d", status))
	var badResp map[string]any
	if err := json.Unmarshal(body, &badResp); err == nil {
		msg := fmt.Sprintf("%v", badResp["errors"])
		check("error points at the missing brake_cylinder column",
			strings.Contains(msg, "brake_cylinder"), msg)
	}

	if failures > 0 {
		fmt.Printf("\nsmoke failed: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nsmoke passed: all checks green")
}

func postVerify(client *http.Client, base, csvBody, specBody string) ([]byte, int, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	cw, err := mw.CreateFormFile("csv", "record.csv")
	if err != nil {
		return nil, 0, err
	}
	if _, err := io.WriteString(cw, csvBody); err != nil {
		return nil, 0, err
	}
	sw, err := mw.CreateFormFile("spec", "phases.json")
	if err != nil {
		return nil, 0, err
	}
	if _, err := io.WriteString(sw, specBody); err != nil {
		return nil, 0, err
	}
	if err := mw.Close(); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/verify", &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func buildCSV(times []int64, trainPipe, brakeCylinder []float64) string {
	var b strings.Builder
	b.WriteString("ms,train_pipe,brake_cylinder\n")
	for i := range times {
		fmt.Fprintf(&b, "%d,%s,%s\n", times[i],
			strconv.FormatFloat(trainPipe[i], 'f', -1, 64),
			strconv.FormatFloat(brakeCylinder[i], 'f', -1, 64))
	}
	return b.String()
}

func constCol(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func intVec(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, len(arr))
	for i, x := range arr {
		parts[i] = strconv.Itoa(int(x.(float64)))
	}
	return strings.Join(parts, ",")
}

func int64Val(v any) int64 {
	f, ok := v.(float64)
	if !ok {
		return -1
	}
	return int64(f)
}

func floatVal(v any) float64 {
	f, _ := v.(float64)
	return f
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "smoke error: "+format+"\n", args...)
	os.Exit(1)
}
