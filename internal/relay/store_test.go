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

	first, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.Create(ctx, "inc-1234", "alice", testVersion)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := first.Create(ctx, "old", "alice", testVersion)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, memberToken := created.Session.ID, created.MemberToken
	if _, err := first.Append(ctx, sessionID, memberToken, "finding", "before restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Stop(ctx, stopped.Session.ID, stopped.MemberToken); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openTestStore(t, path)

	info, err := second.Info(ctx, sessionID, memberToken)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "inc-1234" || info.LatestID != 2 || len(info.Members) != 1 || info.Members[0].Name != "alice" || info.Members[0].MCPVersion != testVersion {
		t.Fatalf("unexpected session after reopen: %+v", info)
	}

	page, err := second.Since(ctx, sessionID, memberToken, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[1].Text != "before restart" || page.Messages[1].Kind != "finding" {
		t.Fatalf("unexpected messages after reopen: %+v", page.Messages)
	}

	next, err := second.Append(ctx, sessionID, memberToken, "update", "after restart")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != 3 || next.From != "alice" {
		t.Fatalf("message after reopen = %+v, want id 3 from alice", next)
	}

	if _, err := second.Info(ctx, stopped.Session.ID, stopped.MemberToken); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stopped session after reopen: err = %v, want ErrSessionNotFound", err)
	}

	if _, err := second.Join(ctx, sessionID, created.SessionToken, "bob", testVersion, ""); err != nil {
		t.Fatalf("session token after reopen: %v", err)
	}
}

func TestStopDeletesMembersAndMessages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "stop.db"))

	created, err := store.Create(ctx, "", "alice", testVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ctx, created.Session.ID, created.SessionToken, "bob", testVersion, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stop(ctx, created.Session.ID, created.MemberToken); err != nil {
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
	store, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	const canary = "CANARY-SECRET-PAYLOAD-12345"
	const memberName = "canary-member-name"
	stopped, err := store.Create(ctx, "canary-session-name", memberName, testVersion)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.Create(ctx, "kept", "bob", testVersion)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if _, err := store.Append(ctx, stopped.Session.ID, stopped.MemberToken, "finding", canary+strings.Repeat("x", 500)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.Stop(ctx, stopped.Session.ID, stopped.MemberToken); err != nil {
		t.Fatal(err)
	}

	forbidden := []string{canary, stopped.Session.ID, memberName, "canary-session-name", hashSecret(stopped.SessionToken), hashSecret(stopped.MemberToken)}
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

	reopened := openTestStore(t, path)
	if _, err := reopened.Info(ctx, kept.Session.ID, kept.MemberToken); err != nil {
		t.Fatalf("other session lost after scrub: %v", err)
	}
}

func TestOpenStoreMigratesOldDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `
		CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_by TEXT NOT NULL, created_at INTEGER NOT NULL, latest_id INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE members (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, name TEXT NOT NULL, joined_at INTEGER NOT NULL, last_seen INTEGER NOT NULL, PRIMARY KEY (session_id, name));
		INSERT INTO meta (key, value) VALUES ('token', 'old-server-token');
		INSERT INTO sessions (id, name, created_by, created_at) VALUES ('s1', 'old', 'alice', 0);
		INSERT INTO members (session_id, name, joined_at, last_seen) VALUES ('s1', 'alice', 0, 0);
	`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t, path)

	var metaTables int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'meta'`).Scan(&metaTables); err != nil {
		t.Fatal(err)
	}
	if metaTables != 0 {
		t.Fatal("meta table with the old server token was not dropped")
	}

	for _, sessionToken := range []string{"", "old-server-token"} {
		if _, err := store.Join(ctx, "s1", sessionToken, "bob", testVersion, ""); !errors.Is(err, ErrBadSessionToken) {
			t.Fatalf("joining a session that has no token with %q: err = %v, want ErrBadSessionToken", sessionToken, err)
		}
	}

	created, err := store.Create(ctx, "new", "carol", testVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Join(ctx, created.Session.ID, created.SessionToken, "dave", testVersion, ""); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentAppendsGetUniqueSequentialIDs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "concurrent.db"))

	created, err := store.Create(ctx, "", "alice", testVersion)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Append(ctx, created.Session.ID, created.MemberToken, "update", "x"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	page, err := store.Since(ctx, created.Session.ID, created.MemberToken, 0, 100)
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
