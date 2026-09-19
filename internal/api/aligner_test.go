package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/arbiter"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/engine"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
)

// Stop really disconnects this backend: the JSON must survive in ASS, not by
// accidentally calling the supposedly dead service and hoping nobody notices.
type alignerSupervisor struct {
	backend *httptest.Server
	started atomic.Bool
	stopped atomic.Bool
}

func (s *alignerSupervisor) EnsureUp(context.Context, string) error {
	s.started.Store(true)
	return nil
}
func (s *alignerSupervisor) BaseURL(string) string { return s.backend.URL }
func (s *alignerSupervisor) Stop(context.Context, string) error {
	s.backend.Close()
	s.stopped.Store(true)
	return nil
}

func TestAlignerUploadHarvestAndStop(t *testing.T) {
	cfg, err := config.Load("../../ass.toml")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := results.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	lyrics := "Hello world\nAnother line"
	audio := []byte("RIFF\x00uploaded vocal audio")
	alignment := `{"language":"en","duration":2.5,"lines":[{"text":"Hello world","start":0.2,"end":0.8,"words":[{"text":"Hello","start":0.2,"end":0.8},{"text":"world","start":null,"end":null}]}]}`
	harvesting, allowHarvest := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	backend := http.NewServeMux()
	backend.HandleFunc("POST /v1/align", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		var p struct {
			Text     string `json:"text"`
			Language string `json:"language"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("params")), &p); err != nil || p.Text != lyrics || p.Language != "en" {
			t.Errorf("forwarded params = %q, error %v", r.FormValue("params"), err)
		}
		f, header, err := r.FormFile("audio")
		if err != nil {
			t.Error(err)
			http.Error(w, "no audio", 400)
			return
		}
		defer f.Close()
		got, err := io.ReadAll(f)
		if err != nil || !bytes.Equal(got, audio) || header.Filename != "vocals.wav" {
			t.Errorf("forwarded audio = %q (%s), error %v", got, header.Filename, err)
		}
		writeJSON(w, 202, map[string]string{"job_id": "backend-alignment", "state": "queued"})
	})
	backend.HandleFunc("GET /v1/jobs/backend-alignment", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"job_id": "backend-alignment", "state": "succeeded", "artifacts": []store.Artifact{
			{Name: "alignment.json", Kind: "metadata", ContentType: "application/json", Bytes: int64(len(alignment))},
		}})
	})
	backend.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"model": "mock-aligner", "vram": map[string]any{"cuda": true, "device": "mock", "allocated_mb": 500, "reserved_mb": 600, "peak_mb": 900}})
	})
	backend.HandleFunc("GET /v1/jobs/backend-alignment/result/alignment.json", func(w http.ResponseWriter, r *http.Request) {
		close(harvesting)
		select {
		case <-allowHarvest:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, alignment)
	})
	sup := &alignerSupervisor{backend: httptest.NewServer(backend)}
	defer sup.backend.Close()
	defer unblock.Do(func() { close(allowHarvest) })
	arb := arbiter.New(sup, cfg)
	eng := engine.New(cfg, arb, st, res)
	api := New(cfg, eng, st, arb).Handler()
	request := func(method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, body)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	params, _ := json.Marshal(map[string]string{"text": lyrics, "language": "en"})
	if err := form.WriteField("params", string(params)); err != nil {
		t.Fatal(err)
	}
	f, err := form.CreateFormFile("audio", "vocals.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	rec := request("POST", "/v1/aligner/jobs", &body, form.FormDataContentType())
	if rec.Code != 202 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	var submitted struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil || submitted.ID == "" {
		t.Fatalf("job: %s (%v)", rec.Body, err)
	}
	select {
	case <-harvesting:
	case <-time.After(5 * time.Second):
		t.Fatal("never reached artifact harvest")
	}
	if !sup.started.Load() {
		t.Fatal("backend wasn't started")
	}
	rec = request("POST", "/v1/backends/aligner/stop", nil, "")
	if rec.Code != 409 {
		t.Fatalf("stop during harvest: %d %s", rec.Code, rec.Body)
	}
	unblock.Do(func() { close(allowHarvest) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec = request("GET", "/v1/jobs/"+submitted.ID, nil, "")
		var job jobView
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.State == store.StateFailed {
			t.Fatal(job.Error)
		}
		if job.State == store.StateSucceeded && arb.Snapshot()["aligner"].Leases == 0 {
			if job.Service != "aligner" || len(job.Artifacts) != 1 || job.Artifacts[0].Name != "alignment.json" || job.Artifacts[0].ContentType != "application/json" || job.Artifacts[0].Kind != "metadata" || job.Artifacts[0].Bytes != int64(len(alignment)) {
				t.Fatalf("wrong result metadata: %+v", job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %s", rec.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	vram, err := st.VRAMSummary(ctx)
	if err != nil || len(vram) != 1 || vram[0].Service != "aligner" || vram[0].MaxPeakMB != 900 {
		t.Fatalf("telemetry: %+v, %v", vram, err)
	}
	rec = request("POST", "/v1/backends/aligner/stop", nil, "")
	if rec.Code != 200 || !sup.stopped.Load() {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	for _, suffix := range []string{"/result/alignment.json", "/result"} {
		rec = request("GET", "/v1/jobs/"+submitted.ID+suffix, nil, "")
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != alignment {
			t.Fatalf("harvested JSON after stop: %d %s", rec.Code, rec.Body)
		}
	}
}
