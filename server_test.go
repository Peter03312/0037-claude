package align

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const validSpecJSON = `{
  "phases": [
    {"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":100000},
     "target_final":600,"abs_tolerance":1000,"allowed_reverse_cumulative":1000000,
     "max_sample_gap_ms":100000},
    {"kind":"release","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":100000},
     "target_final":400,"abs_tolerance":1000,"allowed_reverse_cumulative":1000000,
     "max_sample_gap_ms":100000}
  ]
}`

func TestParseCSVMissingColumn(t *testing.T) {
	_, errs := ParseCSV(strings.NewReader("ms,train_pipe\n0,500\n100,520\n"))
	if len(errs) == 0 {
		t.Fatal("missing brake_cylinder column must be rejected")
	}
	found := false
	for _, e := range errs {
		if e.Location == "header" && strings.Contains(e.Message, ColBrakeCylinder) {
			found = true
		}
	}
	if !found {
		t.Fatalf("error must point at missing column in header, got %v", errs)
	}
}

func TestParseCSVBOMHeader(t *testing.T) {
	ds, errs := ParseCSV(strings.NewReader("\ufeffms,train_pipe,brake_cylinder\n0,500,300\n100,520,300\n"))
	if len(errs) != 0 {
		t.Fatalf("BOM-prefixed header must parse, got %v", errs)
	}
	if ds.NSamp != 2 {
		t.Fatalf("want 2 rows, got %d", ds.NSamp)
	}
}

func TestParseCSVNonIncreasingAndNonFinite(t *testing.T) {
	// 时间非递增 + NaN/Inf 同单报出，整单拒收。
	csv := "ms,train_pipe,brake_cylinder\n" +
		"0,500,300\n" +
		"100,510,300\n" +
		"100,520,300\n" +
		"200,NaN,300\n" +
		"300,530,+Inf\n"
	_, errs := ParseCSV(strings.NewReader(csv))
	if len(errs) < 3 {
		t.Fatalf("want >=3 row/column errors collected in one order, got %v", errs)
	}
	joined := ""
	for _, e := range errs {
		joined += e.Location + " "
	}
	if !strings.Contains(joined, "row 4") {
		t.Fatalf("non-increasing time at row 4 must be reported, got %v", errs)
	}
}

func TestParseCSVNonIntegerTime(t *testing.T) {
	_, errs := ParseCSV(strings.NewReader("ms,train_pipe,brake_cylinder\n0,500,300\n1.5,510,300\n"))
	if len(errs) == 0 || !strings.Contains(errs[0].Location, TimeColumn) {
		t.Fatalf("fractional millisecond must be rejected, got %v", errs)
	}
}

func TestParseCSVTooFewRows(t *testing.T) {
	_, errs := ParseCSV(strings.NewReader("ms,train_pipe,brake_cylinder\n0,500,300\n"))
	if len(errs) == 0 {
		t.Fatal("single data row cannot define a phase interval")
	}
}

func TestParseCSVTimeOutOfBound(t *testing.T) {
	// 超出 int64 安全范围的毫秒时间戳必须在行列上报错，杜绝 int64 溢出。
	_, errs := ParseCSV(strings.NewReader("ms,train_pipe,brake_cylinder\n0,500,300\n99999999999999999999,510,300\n"))
	if len(errs) == 0 {
		t.Fatal("oversized millisecond timestamp must be rejected")
	}
}

func TestParseCSVRaggedRow(t *testing.T) {
	_, errs := ParseCSV(strings.NewReader("ms,train_pipe,brake_cylinder\n0,500,300\n100,510\n200,520,300\n"))
	if len(errs) != 1 || !strings.Contains(errs[0].Location, "row 3") {
		t.Fatalf("ragged row 3 must be reported, got %v", errs)
	}
}

func TestParseSpecValidation(t *testing.T) {
	bad := []struct{ name, js string }{
		{"unknown field", `{"phases":[{"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":10},"target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":0,"holding_band_half_width":2,"max_single_out_of_band_ms":1,"max_total_out_of_band_ms":1,"max_sample_gap_ms":5,"bogus":1}]}`},
		{"bad kind", `{"phases":[{"kind":"explode","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":10},"target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":0,"max_sample_gap_ms":5}]}`},
		{"bad column", `{"phases":[{"kind":"charging","controlled_column":"rpm","duration_ms":{"min_ms":0,"max_ms":10},"target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":0,"max_sample_gap_ms":5}]}`},
		{"inverted duration", `{"phases":[{"kind":"charging","controlled_column":"train_pipe","duration_ms":{"min_ms":20,"max_ms":10},"target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":0,"max_sample_gap_ms":5}]}`},
		{"missing holding fields", `{"phases":[{"kind":"holding","controlled_column":"train_pipe","duration_ms":{"min_ms":0,"max_ms":10},"target_final":100,"abs_tolerance":1,"allowed_reverse_cumulative":0,"max_sample_gap_ms":5}]}`},
		{"empty phases", `{"phases":[]}`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, errs := ParseSpec([]byte(tc.js)); len(errs) == 0 {
				t.Fatalf("spec must be rejected: %s", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// HTTP 层：multipart 入参、400 整单错误、200 qualified/rejected、405。
// ---------------------------------------------------------------------------

func TestHTTPVerifyRoundTrip(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// 缺列 → 400，错误带行列。
	body, ct := multipartBody(t, "ms,train_pipe\n0,500\n100,520\n", validSpecJSON)
	resp, err := http.Post(srv.URL+"/verify", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var errResp InputErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatal(err)
	}
	if len(errResp.Errors) == 0 {
		t.Fatal("400 response must carry errors")
	}

	// 合法整单 → 200 qualified。
	csv := "ms,train_pipe,brake_cylinder\n0,500,300\n100,550,300\n200,600,300\n300,550,300\n400,500,300\n500,450,300\n"
	body, ct = multipartBody(t, csv, validSpecJSON)
	resp, err = http.Post(srv.URL+"/verify", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var result Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "qualified" || len(result.Phases) != 2 {
		t.Fatalf("want qualified with 2 phases, got %+v", result)
	}
	if result.Phases[0].EndSample != result.Phases[1].StartSample {
		t.Fatal("shared boundary must be visible in HTTP response")
	}
}

func TestHTTPMissingPartsAndMethod(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// 非 POST。
	if resp, err := http.Get(srv.URL + "/verify"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("want 405, got %d", resp.StatusCode)
		}
	}

	// 缺 spec 部件。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	cw, err := mw.CreateFormFile("csv", "r.csv")
	if err != nil {
		t.Fatal(err)
	}
	cw.Write([]byte("ms,train_pipe,brake_cylinder\n0,1,1\n10,2,2\n"))
	mw.Close()
	resp, err := http.Post(srv.URL+"/verify", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing spec part, got %d", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

func multipartBody(t *testing.T, csvBody, specBody string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	cw, err := mw.CreateFormFile("csv", "record.csv")
	if err != nil {
		t.Fatal(err)
	}
	cw.Write([]byte(csvBody))
	sw, err := mw.CreateFormFile("spec", "phases.json")
	if err != nil {
		t.Fatal(err)
	}
	sw.Write([]byte(specBody))
	mw.Close()
	return &buf, mw.FormDataContentType()
}
