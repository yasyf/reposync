// Package transfer implements reposync's exact resident snapshot/delta contract.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	dkdaemon "github.com/yasyf/daemonkit/daemon"
	"github.com/yasyf/daemonkit/proc"

	"github.com/yasyf/synckit/cregistry"
	"github.com/yasyf/synckit/hostregistry"
	"github.com/yasyf/synckit/syncservice"

	"github.com/yasyf/reposync/internal/env"
	"github.com/yasyf/reposync/internal/reconcile"
	"github.com/yasyf/reposync/internal/state"
)

const (
	// Identity is the exact product payload and receipt schema.
	Identity    = "reposync-transfer-v1"
	declaration = "payload:{identity:reposync-transfer-v1,version:1,repos:registry,env:map<string,repo_state>};" +
		"delivery:{kind:snapshot|delta,base_revision:uint64,source_revision:uint64,digest:sha256};" +
		"receipt:{origin:string,change_id:sha256,revision:uint64,payload_digest:sha256}"
	ledgerFile = "transfer-v1.json"
	lockFile   = "transfer-v1.lock"
)

// Fingerprint binds the manifest and every delivery to the exact v1 schema.
var Fingerprint = hostregistry.SchemaFingerprint(Identity, declaration)

// Service is reposync's resident Synckit export/apply implementation.
type Service struct{}

type payload struct {
	Identity string                             `json:"identity"`
	Version  uint64                             `json:"version"`
	Repos    cregistry.Registry[state.RepoMeta] `json:"repos"`
	Env      map[string]env.RepoState           `json:"env"`
}

type receipt struct {
	Origin        string               `json:"origin"`
	ChangeID      string               `json:"change_id"`
	Revision      syncservice.Revision `json:"revision"`
	PayloadDigest string               `json:"payload_digest"`
}

type ledger struct {
	Identity     string    `json:"identity"`
	Version      uint64    `json:"version"`
	Source       uint64    `json:"source_revision"`
	SourceDigest string    `json:"source_payload_digest"`
	Applied      []receipt `json:"applied"`
}

// Export returns the immutable current product snapshot as a full or base-fenced
// delta. Revisions advance only when the canonical payload bytes change.
func (Service) Export(ctx context.Context, request syncservice.ExportRequest) (syncservice.ChangeEnvelope, error) {
	if request.ServiceID != state.ToolName || request.SchemaFingerprint != Fingerprint {
		return syncservice.ChangeEnvelope{}, errors.New("reposync transfer: service schema mismatch")
	}
	since, err := request.SinceRevision.Uint64()
	if err != nil {
		return syncservice.ChangeEnvelope{}, err
	}
	var out syncservice.ChangeEnvelope
	err = withLedger(ctx, true, func(l *ledger) error {
		raw, err := exportPayload(ctx)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		encodedDigest := hex.EncodeToString(digest[:])
		if l.SourceDigest != encodedDigest {
			if l.Source == math.MaxUint64 {
				return errors.New("reposync transfer: source revision exhausted")
			}
			l.Source++
			l.SourceDigest = encodedDigest
		}
		if since > l.Source {
			return fmt.Errorf("reposync transfer: requested future revision %d, current %d", since, l.Source)
		}
		kind := syncservice.ChangeDelta
		base := syncservice.NewRevision(since)
		if since == 0 {
			kind = syncservice.ChangeSnapshot
			base = syncservice.NewRevision(0)
		}
		out, err = syncservice.NewExportedChange(
			state.ToolName, Fingerprint, kind, base,
			syncservice.NewRevision(l.Source), raw,
		)
		return err
	})
	return out, err
}

// Apply merges one exact source payload, materializes the resulting local state,
// and records its acknowledgement only after every required write succeeds.
func (Service) Apply(ctx context.Context, change syncservice.ChangeEnvelope) (syncservice.ApplyResult, error) {
	if change.ServiceID != state.ToolName || change.SchemaFingerprint != Fingerprint {
		return syncservice.ApplyResult{}, errors.New("reposync transfer: service schema mismatch")
	}
	if err := change.Validate(true); err != nil {
		return syncservice.ApplyResult{}, err
	}
	var result syncservice.ApplyResult
	err := withLedger(ctx, true, func(l *ledger) error {
		index := receiptIndex(l.Applied, change.Origin)
		current := syncservice.NewRevision(0)
		if index >= 0 {
			current = l.Applied[index].Revision
			held := l.Applied[index]
			if held.ChangeID == change.ChangeID && held.Revision == change.SourceRevision && held.PayloadDigest == change.PayloadDigest {
				result.AckedRevision = held.Revision
				return nil
			}
		}
		currentNumber, err := current.Uint64()
		if err != nil {
			return err
		}
		sourceNumber, err := change.SourceRevision.Uint64()
		if err != nil {
			return err
		}
		if sourceNumber <= currentNumber {
			return errors.New("reposync transfer: stale or conflicting source revision")
		}
		if change.Kind == syncservice.ChangeDelta && change.BaseRevision != current {
			result = syncservice.ApplyResult{AckedRevision: current, NeedSnapshot: true}
			return nil
		}
		incoming, err := decodePayload(change.Payload)
		if err != nil {
			return err
		}
		if err := applyPayload(ctx, incoming); err != nil {
			return err
		}
		next := receipt{
			Origin: change.Origin, ChangeID: change.ChangeID,
			Revision: change.SourceRevision, PayloadDigest: change.PayloadDigest,
		}
		if index < 0 {
			l.Applied = append(l.Applied, next)
		} else {
			l.Applied[index] = next
		}
		result.AckedRevision = change.SourceRevision
		return nil
	})
	return result, err
}

func exportPayload(ctx context.Context) ([]byte, error) {
	var st *state.State
	if err := state.WithLock(ctx, func() error {
		loaded, err := state.Load()
		if err != nil {
			return err
		}
		st = loaded
		return nil
	}); err != nil {
		return nil, err
	}
	dl, err := st.DefaultLocationExpanded()
	if err != nil {
		return nil, err
	}
	configDir, err := state.Dir()
	if err != nil {
		return nil, err
	}
	envState := make(map[string]env.RepoState)
	for _, repo := range st.PropagatingRepos() {
		if repo.NoEnvSync || !reconcile.Present(repo.AbsPath(dl)) {
			continue
		}
		observed, err := reconcile.LocalEnvState(ctx, repo.AbsPath(dl), env.SidecarPath(configDir, repo.Origin), repo.Origin)
		if err != nil {
			return nil, err
		}
		envState[repo.Origin] = observed
	}
	return json.Marshal(payload{Identity: Identity, Version: 1, Repos: st.Repos, Env: envState})
}

func decodePayload(raw []byte) (payload, error) {
	var p payload
	if err := hostregistry.DecodeExactJSON(raw, &p); err != nil {
		return payload{}, fmt.Errorf("reposync transfer: decode payload: %w", err)
	}
	if p.Identity != Identity || p.Version != 1 || p.Repos == nil || p.Env == nil {
		return payload{}, errors.New("reposync transfer: payload schema mismatch")
	}
	if err := reconcile.ValidateEnvSnapshot(p.Env); err != nil {
		return payload{}, err
	}
	for origin := range p.Env {
		entry, ok := p.Repos[origin]
		if !ok || !entry.Present() || entry.Value.LocalOnly || entry.Value.NoEnvSync {
			return payload{}, fmt.Errorf("reposync transfer: env origin %q is not an eligible repo", origin)
		}
	}
	return p, nil
}

func applyPayload(ctx context.Context, incoming payload) error {
	var merged *state.State
	if err := state.WithLock(ctx, func() error {
		st, err := state.Load()
		if err != nil {
			return err
		}
		st.Repos = cregistry.Merge(st.Repos, incoming.Repos)
		if err := st.SaveReposUnlocked(); err != nil {
			return err
		}
		merged = st
		return nil
	}); err != nil {
		return err
	}
	repoResults, err := reconcile.Repos(ctx, merged, merged.AllRepos())
	if err != nil {
		return err
	}
	for _, item := range append(repoResults, reconcile.ApplyEnvSnapshot(ctx, incoming.Env)...) {
		if item.Err != nil {
			return item.Err
		}
		if item.Action == reconcile.ActionEnvBusy {
			return fmt.Errorf("reposync transfer: env for %s is busy", item.Relpath)
		}
	}
	return nil
}

func withLedger(ctx context.Context, write bool, apply func(*ledger) error) error {
	directory, err := state.Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	lock, err := (proc.FileLockSpec{
		Path: filepath.Join(directory, lockFile), Mode: proc.FileLockExclusive,
		Deadline: 30 * time.Second,
	}).Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	path := filepath.Join(directory, ledgerFile)
	l, err := readLedger(path)
	if err != nil {
		return err
	}
	if err := apply(l); err != nil {
		return err
	}
	if !write {
		return nil
	}
	slices.SortFunc(l.Applied, func(a, b receipt) int {
		if a.Origin < b.Origin {
			return -1
		}
		if a.Origin > b.Origin {
			return 1
		}
		return 0
	})
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return dkdaemon.WriteFileDurable(path, append(raw, '\n'), 0o600)
}

func readLedger(path string) (*ledger, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // fixed reposync state path
	if errors.Is(err, os.ErrNotExist) {
		return &ledger{Identity: Identity, Version: 1, Applied: []receipt{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l ledger
	if err := hostregistry.DecodeExactJSON(raw, &l); err != nil {
		return nil, fmt.Errorf("reposync transfer: decode ledger: %w", err)
	}
	if l.Identity != Identity || l.Version != 1 || l.Applied == nil {
		return nil, errors.New("reposync transfer: ledger schema mismatch")
	}
	if l.Source == 0 && l.SourceDigest != "" || l.Source > 0 && !exactDigest(l.SourceDigest) {
		return nil, errors.New("reposync transfer: source ledger is invalid")
	}
	for i, held := range l.Applied {
		if held.Origin == "" || !exactDigest(held.ChangeID) || !exactDigest(held.PayloadDigest) {
			return nil, fmt.Errorf("reposync transfer: receipt %d is invalid", i)
		}
		if _, err := held.Revision.Uint64(); err != nil {
			return nil, err
		}
		if receiptIndex(l.Applied[:i], held.Origin) >= 0 {
			return nil, errors.New("reposync transfer: duplicate receipt origin")
		}
	}
	return &l, nil
}

func receiptIndex(receipts []receipt, origin string) int {
	for i, held := range receipts {
		if held.Origin == origin {
			return i
		}
	}
	return -1
}

func exactDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
