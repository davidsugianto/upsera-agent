package sqlite_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidsugianto/upsera-agent/internal/domain"
	"github.com/davidsugianto/upsera-agent/internal/repository/sqlite"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

var _ investigation.EvidenceStore = (*sqlite.Store)(nil)

func evidence(inv, id, excerpt string) domain.Evidence {
	sum := sha256.Sum256([]byte(excerpt))
	return domain.Evidence{
		ID:              id,
		InvestigationID: inv,
		Tool:            "k8s_get",
		Args:            `{"kind":"deployment","namespace":"prod","name":"api"}`,
		Excerpt:         excerpt,
		TS:              time.Date(2026, 10, 14, 9, 30, 0, 123456789, time.UTC),
		SHA256:          hex.EncodeToString(sum[:]),
	}
}

func openTemp(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "evidence.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	want := evidence("inv-1", "E1", "deployment api: 2/3 ready")
	if err := s.Add(ctx, want); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := s.List(ctx, "inv-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if !got[0].TS.Equal(want.TS) || got[0].TS.Location() != time.UTC {
		t.Errorf("TS = %v, want %v in UTC", got[0].TS, want.TS)
	}
	got[0].TS = want.TS
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestListOrderedByInsertion(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	ids := []string{"E10", "E2", "E1", "E3"}
	for _, id := range ids {
		if err := s.Add(ctx, evidence("inv-1", id, "excerpt "+id)); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	if err := s.Add(ctx, evidence("inv-2", "E1", "other")); err != nil {
		t.Fatalf("Add other: %v", err)
	}
	got, err := s.List(ctx, "inv-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("len = %d, want %d", len(got), len(ids))
	}
	for i, id := range ids {
		if got[i].ID != id {
			t.Errorf("got[%d].ID = %s, want %s", i, got[i].ID, id)
		}
	}
}

func TestAddDuplicateRejected(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	if err := s.Add(ctx, evidence("inv-1", "E1", "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, evidence("inv-1", "E1", "b")); err == nil {
		t.Fatal("duplicate (investigation_id, id) accepted")
	}
	got, _ := s.List(ctx, "inv-1")
	if len(got) != 1 || got[0].Excerpt != "a" {
		t.Errorf("original row changed: %+v", got)
	}
}

func TestAddRejectsInvalid(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	cases := map[string]func(*domain.Evidence){
		"bad hash":         func(e *domain.Evidence) { e.SHA256 = "deadbeef" },
		"empty hash":       func(e *domain.Evidence) { e.SHA256 = "" },
		"excerpt mismatch": func(e *domain.Evidence) { e.Excerpt = "tampered" },
		"empty id":         func(e *domain.Evidence) { e.ID = "" },
		"empty inv id":     func(e *domain.Evidence) { e.InvestigationID = "" },
		"empty tool":       func(e *domain.Evidence) { e.Tool = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := evidence("inv-1", "E1", "ok")
			mutate(&e)
			if err := s.Add(ctx, e); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	got, _ := s.List(ctx, "inv-1")
	if len(got) != 0 {
		t.Errorf("rejected rows were stored: %+v", got)
	}
}

func TestOpenFileMode(t *testing.T) {
	_, path := openTemp(t)
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Dir(path), 0o700)
}

func TestReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	ctx := context.Background()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, evidence("inv-1", "E1", "kept")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.List(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Excerpt != "kept" {
		t.Errorf("after reopen got %+v", got)
	}
	assertMode(t, path, 0o600)
}

func TestListUnknownInvestigationEmpty(t *testing.T) {
	s, _ := openTemp(t)
	got, err := s.List(context.Background(), "inv-missing")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0", len(got))
	}
}

func TestOpenTightensExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	ctx := context.Background()
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, evidence("inv-1", "E1", "survives")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err = sqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	assertMode(t, path, 0o600)
	got, err := s.List(ctx, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Excerpt != "survives" {
		t.Errorf("rows after reopen = %+v", got)
	}
}

func TestOpenTightensEmptyExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // defeat umask
		t.Fatal(err)
	}
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertMode(t, path, 0o600)
}
