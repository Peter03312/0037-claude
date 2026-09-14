package align

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// InputErrorResponse 是缺列、时间不递增、非有限数、规格非法等整单错误的响应。
type InputErrorResponse struct {
	Status string        `json:"status"`
	Errors []ErrorDetail `json:"errors"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

// Handler 是纯后端验算服务的唯一入口（POST /verify，GET /healthz）。
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, InputErrorResponse{
				Status: "error",
				Errors: []ErrorDetail{{Message: "use GET"}},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/verify", handleVerify)
	return mux
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, InputErrorResponse{
			Status: "error",
			Errors: []ErrorDetail{{Message: "use POST multipart/form-data with 'csv' and 'spec' parts"}},
		})
		return
	}

	// 限制整单体积，防止异常大包拖垮检修服务。
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{
			Status: "error",
			Errors: []ErrorDetail{{Message: "cannot parse multipart form: " + err.Error()}},
		})
		return
	}

	csvFile, _, err := r.FormFile("csv")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{
			Status: "error",
			Errors: []ErrorDetail{{Location: "multipart.csv", Message: "missing or unreadable file part: " + err.Error()}},
		})
		return
	}
	defer csvFile.Close()
	csvBytes, err := io.ReadAll(io.LimitReader(csvFile, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{
			Status: "error",
			Errors: []ErrorDetail{{Location: "multipart.csv", Message: "cannot read file: " + err.Error()}},
		})
		return
	}

	specFile, _, err := r.FormFile("spec")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{
			Status: "error",
			Errors: []ErrorDetail{{Location: "multipart.spec", Message: "missing or unreadable file part: " + err.Error()}},
		})
		return
	}
	defer specFile.Close()
	specBytes, err := io.ReadAll(io.LimitReader(specFile, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{
			Status: "error",
			Errors: []ErrorDetail{{Location: "multipart.spec", Message: "cannot read file: " + err.Error()}},
		})
		return
	}

	// 输入不合法即整单拒收，绝不在脏数据上做对齐。
	ds, dsErrs := ParseCSV(bytes.NewReader(csvBytes))
	if len(dsErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{Status: "error", Errors: dsErrs})
		return
	}
	spec, specErrs := ParseSpec(specBytes)
	if len(specErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, InputErrorResponse{Status: "error", Errors: specErrs})
		return
	}

	result := Align(ds, spec)
	// qualified / rejected 都是对有效整单完成验算后的结论，用 200 返回。
	writeJSON(w, http.StatusOK, result)
}
