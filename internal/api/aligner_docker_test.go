//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/arbiter"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/docker"
	"github.com/stevelittlefish/AudioSlopServer/internal/engine"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
	"github.com/stevelittlefish/AudioSlopServer/internal/supervisor"
)

// The real supervisor starts the mock aligner in Docker. No CUDA, no weights,
// and no pretending an in-process handler proves our container wiring works.
func TestAlignerDockerLifecycle(t *testing.T) {
	if _, err := os.Stat(docker.DefaultSocket); err != nil {
		t.Skip("no Docker socket")
	}
	d, err := docker.New(docker.DefaultSocket)
	if err != nil {
		t.Skipf("Docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if ok, err := d.ImageExists(ctx, "ass-mockbackend:local"); err != nil || !ok {
		t.Skipf("build scripts/build-mockbackend.sh first: image available=%v, error=%v", ok, err)
	}
	cfg := &config.Config{GPU: config.GPU{MaxResident: 1}, Services: map[string]config.Service{
		"aligner-smoke": {Image: "ass-mockbackend:local", Port: 18097, Verb: "align", Evict: config.EvictStop, Env: map[string]string{"JOB_MS": "20"}},
	}}
	name := cfg.Services["aligner-smoke"].ContainerName("aligner-smoke")
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.Remove(cleanup, name, true); err != nil {
			t.Errorf("remove test container: %v", err)
		}
	}()
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := results.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(d, cfg)
	arb := arbiter.New(sup, cfg)
	api := New(cfg, engine.New(cfg, arb, st, res), st, arb).Handler()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("params", `{"text":"Hello world","language":"en"}`); err != nil {
		t.Fatal(err)
	}
	f, err := form.CreateFormFile("audio", "vocals.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("mock vocal audio")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/aligner-smoke/jobs", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	var submitted struct {
		ID string `json:"job_id"`
	}
	if rec.Code != 202 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	for {
		job, _, err := st.GetJob(ctx, submitted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == store.StateFailed {
			t.Fatal(job.Error)
		}
		if job.State == store.StateSucceeded && arb.Snapshot()["aligner-smoke"].Leases == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := arb.Stop(ctx, "aligner-smoke"); err != nil {
		t.Fatal(err)
	}
	state, err := d.Inspect(ctx, name)
	if err != nil || state.Running {
		t.Fatalf("container after stop: %+v (%v)", state, err)
	}
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs/"+submitted.ID+"/result/alignment.json", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	var alignment struct {
		Language string `json:"language"`
		Lines    []struct {
			Words []struct {
				Start *float64 `json:"start"`
			} `json:"words"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &alignment); err != nil {
		t.Fatal(err)
	}
	if alignment.Language != "en" || len(alignment.Lines) != 1 || len(alignment.Lines[0].Words) != 2 || alignment.Lines[0].Words[1].Start != nil {
		t.Fatalf("unexpected alignment: %s", rec.Body)
	}
}
