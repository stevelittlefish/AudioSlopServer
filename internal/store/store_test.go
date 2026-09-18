package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestVRAMSamples(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Two demucs readings + one yue reading; the summary should roll each service
	// up to its per-field MAX (the number that matters for the budget).
	must := func(service string, v VRAMSample) {
		if err := st.RecordVRAM(ctx, service, "job1", v); err != nil {
			t.Fatalf("RecordVRAM: %v", err)
		}
	}
	must("demucs", VRAMSample{AllocatedMB: 1000, ReservedMB: 1200, PeakMB: 1500})
	must("demucs", VRAMSample{AllocatedMB: 1100, ReservedMB: 1100, PeakMB: 1800})
	must("yue", VRAMSample{AllocatedMB: 7000, ReservedMB: 7500, PeakMB: 8200})

	sums, err := st.VRAMSummary(ctx)
	if err != nil {
		t.Fatalf("VRAMSummary: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("want 2 services, got %d: %+v", len(sums), sums)
	}
	byName := map[string]VRAMSummary{sums[0].Service: sums[0], sums[1].Service: sums[1]}
	if d := byName["demucs"]; d.Samples != 2 || d.MaxPeakMB != 1800 || d.MaxReservedMB != 1200 {
		t.Fatalf("demucs rollup wrong: %+v", d)
	}
	if y := byName["yue"]; y.Samples != 1 || y.MaxPeakMB != 8200 {
		t.Fatalf("yue rollup wrong: %+v", y)
	}
}

func TestVRAMSummaryEmpty(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	sums, err := st.VRAMSummary(context.Background())
	if err != nil {
		t.Fatalf("VRAMSummary: %v", err)
	}
	if len(sums) != 0 {
		t.Fatalf("want empty, got %+v", sums)
	}
}

func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	j, err := st.CreateJob(ctx, "demucs")
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if j.ID == "" || j.State != StateQueued {
		t.Fatalf("fresh job looks wrong: %+v", j)
	}

	if err := st.MarkRunning(ctx, j.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := st.AddArtifact(ctx, j.ID, Artifact{
		Name: "vocals", Kind: "stem", ContentType: "audio/wav", Path: "/x/vocals.wav", Bytes: 60,
	}); err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	if err := st.MarkSucceeded(ctx, j.ID); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	got, arts, err := st.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.State != StateSucceeded {
		t.Fatalf("state = %q, want succeeded", got.State)
	}
	if got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("timestamps not set: %+v", got)
	}
	if len(arts) != 1 || arts[0].Name != "vocals" || arts[0].Bytes != 60 {
		t.Fatalf("artifacts wrong: %+v", arts)
	}

	a, err := st.GetArtifact(ctx, j.ID, "vocals")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if a.Path != "/x/vocals.wav" {
		t.Fatalf("artifact path = %q", a.Path)
	}

	if _, _, err := st.GetJob(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJob(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := st.GetArtifact(ctx, j.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArtifact(unknown) = %v, want ErrNotFound", err)
	}
}
