package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

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
