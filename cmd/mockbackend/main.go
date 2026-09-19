// Command mockbackend is a fake audio backend: it speaks the ASS backend
// contract (/health, /v1/<verb>, /v1/jobs/{id}, result, /park, /unpark) but does
// no real audio work and, crucially, needs no GPU. It exists so we can develop
// and test the entire orchestrator — supervisor, arbiter, proxy, swapping — on a
// machine with no graphics card, which is exactly the machine we're on.
//
// It is not slop for the masses; it's a crash-test dummy. But even dummies get
// comments (rule 2), so enjoy.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// artifact mirrors the ASS artifact model: a named, typed output file.
type artifact struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
}

// job is one pretend unit of work that "runs" for jobDuration and then succeeds,
// producing a set of artifacts appropriate to its verb.
type job struct {
	ID         string     `json:"job_id"`
	State      string     `json:"state"` // queued -> running -> succeeded
	Verb       string     `json:"verb"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt time.Time  `json:"finished_at,omitempty"`
	Artifacts  []artifact `json:"artifacts"`
}

// artifactsForVerb fakes plausible outputs so ASS's harvest path sees the real
// shape: DEMUCS-like multi-stem, YuE-like mixed audio+score, or a lone file.
func artifactsForVerb(verb string) []artifact {
	switch verb {
	case "separate":
		return []artifact{
			{Name: "vocals.wav", Kind: "stem", ContentType: "audio/wav", Bytes: int64(len(silentWAV()))},
			{Name: "no_vocals.wav", Kind: "stem", ContentType: "audio/wav", Bytes: int64(len(silentWAV()))},
		}
	case "align":
		return []artifact{
			{Name: "alignment.json", Kind: "metadata", ContentType: "application/json", Bytes: int64(len(fakeAlignment()))},
		}
	case "generate":
		return []artifact{
			{Name: "audio.flac", Kind: "audio", ContentType: "audio/flac", Bytes: int64(len(silentWAV()))},
			{Name: "score.abc", Kind: "score", ContentType: "text/vnd.abc", Bytes: int64(len(fakeABC()))},
		}
	default:
		return []artifact{
			{Name: "output.wav", Kind: "audio", ContentType: "audio/wav", Bytes: int64(len(silentWAV()))},
		}
	}
}

// bytesForArtifact returns the pretend contents of a named artifact.
func bytesForArtifact(name string) ([]byte, string, bool) {
	switch {
	case name == "alignment.json":
		return fakeAlignment(), "application/json", true
	case strings.HasSuffix(name, ".abc"):
		return fakeABC(), "text/vnd.abc", true
	case strings.HasSuffix(name, ".wav"), strings.HasSuffix(name, ".flac"):
		return silentWAV(), "audio/wav", true // it's really a WAV; close enough for a dummy
	default:
		return nil, "", false
	}
}

// fakeAlignment is a fixed fixture, not an acoustic miracle from a GPU-less box.
// Null timings exercise the unresolved-word case without inventing confidence.
func fakeAlignment() []byte {
	return []byte(`{"language":"en","duration":2.5,"lines":[{"text":"Hello world","start":0.2,"end":0.8,"words":[{"text":"Hello","start":0.2,"end":0.8},{"text":"world","start":null,"end":null}]}]}`)
}

// fakeABC is a tiny valid-ish ABC score, so the "not everything is audio" path
// carries real non-audio bytes.
func fakeABC() []byte {
	return []byte("X:1\nT:Slop in C\nM:4/4\nK:C\nCDEF GABc|\n")
}

var (
	mu       sync.Mutex
	jobs     = map[string]*job{}
	nextID   int
	parked   bool // flipped by /park and /unpark; surfaced in /health and /v1/info
	verbName = env("VERB", "generate")
	// How long a pretend job takes. Short by default so tests are quick.
	jobDuration = time.Duration(envInt("JOB_MS", 500)) * time.Millisecond
)

func main() {
	addr := env("ADDR", ":"+env("PORT", "8000"))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/v1/info", handleInfo)
	mux.HandleFunc("/v1/"+verbName, handleSubmit)
	mux.HandleFunc("/v1/jobs/", handleJob) // /v1/jobs/{id} and /v1/jobs/{id}/result
	mux.HandleFunc("/park", handlePark)
	mux.HandleFunc("/unpark", handleUnpark)

	log.Printf("[mockbackend] verb=%q addr=%q job_ms=%d — pretending to be a GPU, badly",
		verbName, addr, jobDuration.Milliseconds())
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[mockbackend] %v", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	// A parked backend is still "up" for readiness; it just has no weights on the
	// GPU. ASS unparks it before sending work.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "parked": isParked()})
}

func handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"model":  "mock-" + verbName,
		"device": "cpu (there is no GPU, that's the whole point)",
		"verb":   verbName,
		"parked": isParked(),
		// Synthetic VRAM so ASS's /v1/vram telemetry path can be exercised on the
		// GPU-less dev box. A per-verb base (so services differ) plus a little
		// jitter (so peaks vary), clearly labelled mock — nobody should mistake
		// these for real numbers.
		"vram": mockVRAM(),
	})
}

// mockVRAM fabricates a plausible-but-fake VRAM reading. The base scales with the
// verb name's length purely so different mock services report different sizes.
func mockVRAM() map[string]any {
	base := 800 + len(verbName)*300
	jitter := int(time.Now().UnixNano()/1e6) % 250
	return map[string]any{
		"cuda":         true,
		"device":       "cuda:0 (mock — no real card)",
		"allocated_mb": base + jitter,
		"reserved_mb":  base + 300 + jitter,
		"peak_mb":      base + 600 + jitter,
	}
}

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	nextID++
	j := &job{
		ID:        "job-" + strconv.Itoa(nextID),
		State:     "queued",
		Verb:      verbName,
		CreatedAt: time.Now(),
	}
	jobs[j.ID] = j
	mu.Unlock()

	// "Run" it in the background, because real generation is async and callers
	// poll. We honor that fiction faithfully.
	go func() {
		mu.Lock()
		j.State = "running"
		mu.Unlock()
		time.Sleep(jobDuration)
		mu.Lock()
		j.State = "succeeded"
		j.FinishedAt = time.Now()
		j.Artifacts = artifactsForVerb(j.Verb) // outputs only exist once done
		mu.Unlock()
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": j.ID, "state": "queued"})
}

func handleJob(w http.ResponseWriter, r *http.Request) {
	// Path is /v1/jobs/{id} or /v1/jobs/{id}/result/{name}.
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	id := rest
	var tail string // everything after the id, e.g. "result/vocals.wav"
	if i := strings.Index(rest, "/"); i >= 0 {
		id = rest[:i]
		tail = strings.TrimPrefix(rest[i:], "/")
	}

	mu.Lock()
	j, ok := jobs[id]
	mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}

	// Artifact download: /v1/jobs/{id}/result/{name}
	if name, isResult := strings.CutPrefix(tail, "result/"); isResult {
		mu.Lock()
		done := j.State == "succeeded"
		mu.Unlock()
		if !done {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "job not finished", "state": j.State})
			return
		}
		body, ctype, ok := bytesForArtifact(name)
		if !ok {
			http.Error(w, "no such artifact", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	// Otherwise: job status (including the artifact list once succeeded).
	mu.Lock()
	defer mu.Unlock()
	writeJSON(w, http.StatusOK, j)
}

func handlePark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	setParked(true)
	log.Printf("[mockbackend] parked — pretending to move weights to CPU RAM")
	writeJSON(w, http.StatusOK, map[string]any{"parked": true})
}

func handleUnpark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	setParked(false)
	log.Printf("[mockbackend] unparked — pretending to move weights back to the GPU we don't have")
	writeJSON(w, http.StatusOK, map[string]any{"parked": false})
}

// --- little helpers -------------------------------------------------------

func isParked() bool   { mu.Lock(); defer mu.Unlock(); return parked }
func setParked(v bool) { mu.Lock(); defer mu.Unlock(); parked = v }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// silentWAV returns a minimal valid 44-byte WAV header plus a handful of silent
// samples. Enough for a client to say "yes, that's a WAV" and move on.
func silentWAV() []byte {
	const samples = 8
	dataLen := samples * 2 // 16-bit mono
	buf := make([]byte, 0, 44+dataLen)
	put := func(s string) { buf = append(buf, s...) }
	putU32 := func(v uint32) { buf = append(buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	putU16 := func(v uint16) { buf = append(buf, byte(v), byte(v>>8)) }

	put("RIFF")
	putU32(uint32(36 + dataLen))
	put("WAVE")
	put("fmt ")
	putU32(16)        // PCM fmt chunk size
	putU16(1)         // PCM
	putU16(1)         // mono
	putU32(44100)     // sample rate
	putU32(44100 * 2) // byte rate
	putU16(2)         // block align
	putU16(16)        // bits per sample
	put("data")
	putU32(uint32(dataLen))
	buf = append(buf, make([]byte, dataLen)...) // silence
	return buf
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("[mockbackend] ignoring bad %s=%q, using %d", key, v, def)
	}
	return def
}
