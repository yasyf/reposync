package transfer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/state"
)

func initialize(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := state.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
}

func exportRequest(revision uint64) syncservice.ExportRequest {
	return syncservice.ExportRequest{
		ServiceID: state.ToolName, SchemaFingerprint: Fingerprint,
		SinceRevision: syncservice.NewRevision(revision),
	}
}

func TestExportAdvancesOnlyForPropagatingPayloadChanges(t *testing.T) {
	initialize(t)
	service := Service{}

	first, err := service.Export(t.Context(), exportRequest(0))
	if err != nil {
		t.Fatalf("initial export: %v", err)
	}
	if first.Kind != syncservice.ChangeSnapshot || first.BaseRevision != "0" || first.SourceRevision != "1" {
		t.Fatalf("initial change = %+v", first)
	}

	unchanged, err := service.Export(t.Context(), exportRequest(1))
	if err != nil {
		t.Fatalf("unchanged export: %v", err)
	}
	if unchanged.Kind != syncservice.ChangeDelta || unchanged.BaseRevision != "1" || unchanged.SourceRevision != "1" {
		t.Fatalf("unchanged change = %+v", unchanged)
	}

	if _, err := state.Update(t.Context(), func(st *state.State) error {
		st.AddRepo(state.Repo{Relpath: "scratch", LocalOnly: true})
		return nil
	}); err != nil {
		t.Fatalf("add local repo: %v", err)
	}
	localOnly, err := service.Export(t.Context(), exportRequest(1))
	if err != nil {
		t.Fatalf("local-only export: %v", err)
	}
	if localOnly.SourceRevision != "1" {
		t.Fatalf("local-only revision = %s, want 1", localOnly.SourceRevision)
	}

	if _, err := state.Update(t.Context(), func(st *state.State) error {
		st.AddRepo(state.Repo{Relpath: "shared", Origin: "https://example.com/shared.git", Trunk: "main"})
		return nil
	}); err != nil {
		t.Fatalf("add shared repo: %v", err)
	}
	changed, err := service.Export(t.Context(), exportRequest(1))
	if err != nil {
		t.Fatalf("changed export: %v", err)
	}
	if changed.Kind != syncservice.ChangeDelta || changed.BaseRevision != "1" || changed.SourceRevision != "2" {
		t.Fatalf("changed change = %+v", changed)
	}
}

func TestApplyIsIdempotentAndBaseFenced(t *testing.T) {
	initialize(t)
	service := Service{}
	payloadBytes, err := json.Marshal(payload{
		Identity: Identity, Version: 1,
		Repos: cregistry.New[state.RepoMeta](), Env: map[string]env.RepoState{},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	change, err := syncservice.NewExportedChange(
		state.ToolName, Fingerprint, syncservice.ChangeSnapshot,
		syncservice.NewRevision(0), syncservice.NewRevision(1), payloadBytes,
	)
	if err != nil {
		t.Fatalf("new snapshot: %v", err)
	}
	change, err = syncservice.BindDelivery(change, "yasyf@source")
	if err != nil {
		t.Fatalf("bind snapshot: %v", err)
	}
	ack, err := service.Apply(t.Context(), change)
	if err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}
	if ack.NeedSnapshot || ack.AckedRevision != "1" {
		t.Fatalf("snapshot ack = %+v", ack)
	}
	replay, err := service.Apply(t.Context(), change)
	if err != nil {
		t.Fatalf("replay snapshot: %v", err)
	}
	if replay != ack {
		t.Fatalf("replay ack = %+v, want %+v", replay, ack)
	}

	delta, err := syncservice.NewExportedChange(
		state.ToolName, Fingerprint, syncservice.ChangeDelta,
		syncservice.NewRevision(0), syncservice.NewRevision(2), payloadBytes,
	)
	if err != nil {
		t.Fatalf("new delta: %v", err)
	}
	delta, err = syncservice.BindDelivery(delta, "yasyf@source")
	if err != nil {
		t.Fatalf("bind delta: %v", err)
	}
	mismatch, err := service.Apply(context.Background(), delta)
	if err != nil {
		t.Fatalf("apply mismatched delta: %v", err)
	}
	if !mismatch.NeedSnapshot || mismatch.AckedRevision != "1" {
		t.Fatalf("mismatch ack = %+v", mismatch)
	}
}
