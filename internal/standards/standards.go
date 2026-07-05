package standards

import (
	"encoding/json"
	"net/http"
)

const (
	MediaTypeJSON        = "application/json; charset=utf-8"
	MediaTypeProblemJSON = "application/problem+json; charset=utf-8"
	MediaTypeCSV         = "text/csv; charset=utf-8"
	MediaTypeTSV         = "text/tab-separated-values; charset=utf-8"
	MediaTypeXML         = "application/xml; charset=utf-8"
	MediaTypeXLSX        = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Error keeps compatibility with existing browser code and tests.
	Error    string `json:"error,omitempty"`
	Instance string `json:"instance,omitempty"`
}

func NewProblem(status int, title, detail, instance string) Problem {
	if title == "" {
		title = http.StatusText(status)
	}
	return Problem{
		Type:     "about:blank",
		Title:    title,
		Status:   status,
		Detail:   detail,
		Error:    detail,
		Instance: instance,
	}
}

func WriteProblem(w http.ResponseWriter, p Problem) {
	w.Header().Set("Content-Type", MediaTypeProblemJSON)
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
