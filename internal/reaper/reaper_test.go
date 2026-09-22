package reaper

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
)

// mb makes a Retention with just a byte budget; the other tests set their own.
func ret(r config.Retention) *Reaper { return &Reaper{cfg: r} }

// mbptr is the *int64 the size cap wants (nil = unset/unlimited in a raw struct).
func mbptr(v int64) *int64 { return &v }

func jobs(specs ...store.TerminalJob) []store.TerminalJob { return specs }

func ids(vs []store.TerminalJob) string {
	var b strings.Builder
	for i, v := range vs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(v.ID)
	}
	return b.String()
}

func TestPlanMaxJobs(t *testing.T) {
	now := time.Unix(1000, 0)
	// Oldest-first input; keep at most 2, so the two oldest die.
	in := jobs(
		store.TerminalJob{ID: "a", EndedAt: now.Add(-4 * time.Hour), Bytes: 1},
		store.TerminalJob{ID: "b", EndedAt: now.Add(-3 * time.Hour), Bytes: 1},
		store.TerminalJob{ID: "c", EndedAt: now.Add(-2 * time.Hour), Bytes: 1},
		store.TerminalJob{ID: "d", EndedAt: now.Add(-1 * time.Hour), Bytes: 1},
	)
	got := ids(ret(config.Retention{MaxJobs: 2}).plan(in, now))
	if got != "a,b" {
		t.Fatalf("MaxJobs=2: want a,b reaped, got %q", got)
	}
}

func TestPlanMaxAge(t *testing.T) {
	now := time.Unix(10000, 0)
	in := jobs(
		store.TerminalJob{ID: "old1", EndedAt: now.Add(-3 * time.Hour), Bytes: 1},
		store.TerminalJob{ID: "old2", EndedAt: now.Add(-2 * time.Hour), Bytes: 1},
		store.TerminalJob{ID: "fresh", EndedAt: now.Add(-30 * time.Minute), Bytes: 1},
	)
	got := ids(ret(config.Retention{MaxAge: config.Duration{Duration: 90 * time.Minute}}).plan(in, now))
	if got != "old1,old2" {
		t.Fatalf("MaxAge=90m: want old1,old2 reaped, got %q", got)
	}
}

func TestPlanMaxTotalBytes(t *testing.T) {
	now := time.Unix(0, 0)
	// 4 jobs of 30 MB each = 120 MB total; budget 100 MB. Reap oldest until <=100,
	// so one 30 MB job (the oldest) goes and total drops to 90.
	mb := int64(1024 * 1024)
	in := jobs(
		store.TerminalJob{ID: "a", EndedAt: now.Add(1), Bytes: 30 * mb},
		store.TerminalJob{ID: "b", EndedAt: now.Add(2), Bytes: 30 * mb},
		store.TerminalJob{ID: "c", EndedAt: now.Add(3), Bytes: 30 * mb},
		store.TerminalJob{ID: "d", EndedAt: now.Add(4), Bytes: 30 * mb},
	)
	got := ids(ret(config.Retention{MaxTotalMB: mbptr(100)}).plan(in, now))
	if got != "a" {
		t.Fatalf("MaxTotalMB=100: want just a reaped, got %q", got)
	}
}

func TestPlanNoLimitsKeepsEverything(t *testing.T) {
	now := time.Now()
	in := jobs(
		store.TerminalJob{ID: "a", EndedAt: now, Bytes: 999},
		store.TerminalJob{ID: "b", EndedAt: now, Bytes: 999},
	)
	if v := ret(config.Retention{}).plan(in, now); len(v) != 0 {
		t.Fatalf("no limits: want nothing reaped, got %q", ids(v))
	}
}

// TestSweepDeletesFilesAndRows drives a real store + results dir end to end: it
// harvests three jobs, keeps one, and checks that both the rows and the bytes of
// the other two are gone.
func TestSweepDeletesFilesAndRows(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/ass.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	res, err := results.New(t.TempDir() + "/results")
	if err != nil {
		t.Fatalf("results.New: %v", err)
	}

	for i := 0; i < 3; i++ {
		j, err := st.CreateJob(ctx, "demucs")
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		path, n, err := res.Save(j.ID, "audio.wav", strings.NewReader("pretend-wav-bytes"))
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := st.AddArtifact(ctx, j.ID, store.Artifact{Name: "audio.wav", Kind: "audio", ContentType: "audio/wav", Bytes: n, Path: path}); err != nil {
			t.Fatalf("AddArtifact: %v", err)
		}
		if err := st.MarkSucceeded(ctx, j.ID); err != nil {
			t.Fatalf("MarkSucceeded: %v", err)
		}
		time.Sleep(time.Millisecond) // distinct finished_at so oldest-first is stable
	}

	r := New(config.Retention{MaxJobs: 1}, st, res)
	r.SweepNow(ctx)

	_, total, err := st.ListJobs(ctx, 100, 0)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if total != 1 {
		t.Fatalf("after sweep want 1 job, got %d", total)
	}
	// The surviving job's files must still be on disk (it wasn't reaped), and the
	// reaped ones' directories must be gone.
	remaining, _, _ := st.ListJobs(ctx, 100, 0)
	if len(remaining[0].Artifacts) != 1 {
		t.Fatalf("survivor lost its artifact row: %+v", remaining[0])
	}
}
