package relay

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStatePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persist.db")

	first, err := OpenStore(ctx, path, 10)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(ctx, "inc-1234", "alice")
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := first.Create(ctx, "old", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Join(ctx, created.ID, "alice", testVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(ctx, created.ID, "alice", "finding", "before restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Stop(ctx, stopped.ID); err != nil {
		t.Fatal(err)
	}
	if err := first.SetToken(ctx, "persisted-token"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openTestStore(t, path, 10)

	info, err := second.Info(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "inc-1234" || info.LatestID != 2 || len(info.Members) != 1 || info.Members[0].Name != "alice" || info.Members[0].MCPVersion != testVersion {
		t.Fatalf("unexpected session after reopen: %+v", info)
	}

	page, err := second.Since(ctx, created.ID, "alice", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[1].Text != "before restart" || page.Messages[1].Kind != "finding" {
		t.Fatalf("unexpected messages after reopen: %+v", page.Messages)
	}

	next, err := second.Append(ctx, created.ID, "alice", "update", "after restart")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != 3 {
		t.Fatalf("id after reopen = %d, want 3", next.ID)
	}

	if _, err := second.Info(ctx, stopped.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stopped session after reopen: err = %v, want ErrSessionNotFound", err)
	}

	token, err := second.Token(ctx)
	if err != nil || token != "persisted-token" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
}

func TestStopDeletesMembersAndMessages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "stop.db"), 10)

	created, err := store.Create(ctx, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ctx, created.ID, "alice", testVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stop(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	for table, want := range map[string]int{"sessions": 0, "members": 0, "messages": 0} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s has %d rows after stop, want %d", table, count, want)
		}
	}
}

func TestStopLeavesNoSessionDataOnDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scrub.db")
	store, err := OpenStore(ctx, path, 10)
	if err != nil {
		t.Fatal(err)
	}

	const canary = "CANARY-SECRET-PAYLOAD-12345"
	const memberName = "canary-member-name"
	stopped, err := store.Create(ctx, "canary-session-name", memberName)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.Create(ctx, "kept", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ctx, stopped.ID, memberName, testVersion); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, err := store.Append(ctx, stopped.ID, memberName, "finding", canary+strings.Repeat("x", 500)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Join(ctx, kept.ID, "bob", testVersion); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Stop(ctx, stopped.ID); err != nil {
		t.Fatal(err)
	}

	forbidden := []string{canary, stopped.ID, memberName, "canary-session-name"}
	assertNotOnDisk := func(when string) {
		t.Helper()
		for _, file := range []string{path, path + "-wal"} {
			contents, err := os.ReadFile(file)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range forbidden {
				if bytes.Contains(contents, []byte(value)) {
					t.Errorf("%s: %s still contains %q", when, filepath.Base(file), value)
				}
			}
		}
	}

	assertNotOnDisk("while open")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	assertNotOnDisk("after close")

	reopened := openTestStore(t, path, 10)
	if _, err := reopened.Info(ctx, kept.ID); err != nil {
		t.Fatalf("other session lost after scrub: %v", err)
	}
}

func TestOpenStoreAddsVersionColumnToOldDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `
		CREATE TABLE sessions (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_by TEXT NOT NULL, created_at INTEGER NOT NULL, latest_id INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE members (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, name TEXT NOT NULL, joined_at INTEGER NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY (session_id, name));
		INSERT INTO sessions (id, name, created_by, created_at) VALUES ('s1', 'old', 'alice', 0);
		INSERT INTO members (session_id, name, joined_at, last_seen) VALUES ('s1', 'alice', 0, 0);
	`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t, path, 10)
	info, err := store.Info(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Members) != 1 || info.Members[0].MCPVersion != "" {
		t.Fatalf("unexpected members after migration: %+v", info.Members)
	}
	joined, err := store.Join(ctx, "s1", "alice", testVersion)
	if err != nil {
		t.Fatal(err)
	}
	if joined.Session.Members[0].MCPVersion != testVersion {
		t.Fatalf("version after rejoin = %q, want %q", joined.Session.Members[0].MCPVersion, testVersion)
	}
}

func TestTokenEmptyWhenUnset(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "token.db"), 10)
	token, err := store.Token(context.Background())
	if err != nil || token != "" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
}

func TestConcurrentAppendsGetUniqueSequentialIDs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "concurrent.db"), 10)

	created, err := store.Create(ctx, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ctx, created.ID, "alice", testVersion); err != nil {
		t.Fatal(err)
	}

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Append(ctx, created.ID, "alice", "update", "x"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	page, err := store.Since(ctx, created.ID, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != writers+1 || page.LatestID != writers+1 {
		t.Fatalf("got %d messages, latestId %d, want %d", len(page.Messages), page.LatestID, writers+1)
	}
	for i, message := range page.Messages {
		if message.ID != int64(i+1) {
			t.Fatalf("message %d has id %d", i, message.ID)
		}
	}
}
