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

// job is one pretend unit of work that "runs" for jobDuration and then succeeds.
type job struct {
	ID         string    `json:"job_id"`
	State      string    `json:"state"` // queued -> running -> succeeded
	Verb       string    `json:"verb"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
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
	})
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
		mu.Unlock()
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": j.ID, "state": "queued"})
}

func handleJob(w http.ResponseWriter, r *http.Request) {
	// Path is /v1/jobs/{id} or /v1/jobs/{id}/result.
	rest := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	id := rest
	wantResult := false
	if i := strings.Index(rest, "/"); i >= 0 {
		id = rest[:i]
		wantResult = strings.TrimPrefix(rest[i:], "/") == "result"
	}

	mu.Lock()
	j, ok := jobs[id]
	mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}

	if wantResult {
		mu.Lock()
		done := j.State == "succeeded"
		mu.Unlock()
		if !done {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "job not finished", "state": j.State})
			return
		}
		// Hand back a tiny but valid WAV so the proxy path moves real bytes.
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(silentWAV())
		return
	}

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
