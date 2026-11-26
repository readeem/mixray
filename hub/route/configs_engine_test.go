package route

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/readeem/mixray/adapter/xray"
	C "github.com/readeem/mixray/constant"

	"github.com/metacubex/http"
)

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), code: http.StatusOK}
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *responseRecorder) WriteHeader(code int)        { r.code = code }

func TestConfigsEndpointGetsAndPatchesEngine(t *testing.T) {
	original := xray.Engine()
	xray.SetEngine(C.EngineXray)
	t.Cleanup(func() { xray.SetEngine(original) })

	router := configRouter()

	getEngine := func() string {
		t.Helper()
		rec := newResponseRecorder()
		req, err := http.NewRequest(http.MethodGet, "/", nil)
		if err != nil {
			t.Fatal(err)
		}
		router.ServeHTTP(rec, req)
		if rec.code != http.StatusOK {
			t.Fatalf("GET /configs status = %d", rec.code)
		}
		var body struct {
			Engine string `json:"engine"`
		}
		if err := json.Unmarshal(rec.body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Engine
	}

	if got := getEngine(); got != "xray" {
		t.Fatalf("initial engine = %q, want xray", got)
	}

	rec := newResponseRecorder()
	req, err := http.NewRequest(http.MethodPatch, "/", bytes.NewBufferString(`{"engine":"mihomo"}`))
	if err != nil {
		t.Fatal(err)
	}
	router.ServeHTTP(rec, req)
	if rec.code != http.StatusNoContent {
		t.Fatalf("PATCH /configs status = %d, body = %s", rec.code, rec.body.String())
	}
	if got := getEngine(); got != "mihomo" {
		t.Fatalf("patched engine = %q, want mihomo", got)
	}

	rec = newResponseRecorder()
	req, err = http.NewRequest(http.MethodPatch, "/", bytes.NewBufferString(`{"engine":"bogus"}`))
	if err != nil {
		t.Fatal(err)
	}
	router.ServeHTTP(rec, req)
	if rec.code != http.StatusBadRequest {
		t.Fatalf("invalid PATCH /configs status = %d, want 400", rec.code)
	}
}
