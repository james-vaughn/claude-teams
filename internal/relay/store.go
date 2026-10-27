package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hudl/claude-teams/internal/team"
)

var (
	ErrSessionNotFound = errors.New("session not found or already stopped")
	ErrBadSessionToken = errors.New("session token is missing or wrong; ask whoever started the session for it")
	ErrNotMember       = errors.New("send the member token you got when you joined this session")
	ErrNameTaken       = errors.New("another member already uses this name in the session; pick a different name")
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	latest_id  INTEGER NOT NULL DEFAULT 0,
	token_hash TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS members (
	session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	mcp_version TEXT NOT NULL DEFAULT '',
	secret_hash TEXT NOT NULL DEFAULT '',
	joined_at   INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	PRIMARY KEY (session_id, name)
);

CREATE TABLE IF NOT EXISTS messages (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	id         INTEGER NOT NULL,
	sender     TEXT NOT NULL,
	kind       TEXT NOT NULL,
	text       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (session_id, id)
);
`

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "foreign_keys(1)")
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "synchronous(NORMAL)")
	pragmas.Add("_pragma", "secure_delete(1)")

	db, err := sql.Open("sqlite", "file:"+path+"?"+pragmas.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema in %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema in %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	for _, column := range []struct{ table, name string }{
		{"sessions", "token_hash"},
		{"members", "mcp_version"},
		{"members", "secret_hash"},
	} {
		if err := addTextColumn(ctx, db, column.table, column.name); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS meta`)
	return err
}

func addTextColumn(ctx context.Context, db *sql.DB, table, column string) error {
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` TEXT NOT NULL DEFAULT ''`)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Create(ctx context.Context, name, createdBy, mcpVersion string) (team.CreateSessionResponse, error) {
	id, err := newUUID()
	if err != nil {
		return team.CreateSessionResponse{}, err
	}
	sessionToken, err := newSecret()
	if err != nil {
		return team.CreateSessionResponse{}, err
	}

	var joined team.JoinResponse
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, name, created_by, created_at, token_hash) VALUES (?, ?, ?, ?, ?)`,
			id, name, createdBy, s.timestamp(), hashSecret(sessionToken)); err != nil {
			return err
		}
		joined, err = s.joinMember(ctx, tx, id, createdBy, mcpVersion, "")
		return err
	})
	if err != nil {
		return team.CreateSessionResponse{}, err
	}
	return team.CreateSessionResponse{JoinResponse: joined, SessionToken: sessionToken}, nil
}

func (s *Store) Info(ctx context.Context, id, memberToken string) (team.SessionInfo, error) {
	var info team.SessionInfo
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireSession(ctx, tx, id); err != nil {
			return err
		}
		if _, err := s.authenticateMember(ctx, tx, id, memberToken); err != nil {
			return err
		}
		var err error
		info, err = sessionInfo(ctx, tx, id)
		return err
	})
	return info, err
}

func (s *Store) Stop(ctx context.Context, id, memberToken string) (team.SessionInfo, error) {
	var info team.SessionInfo
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireSession(ctx, tx, id); err != nil {
			return err
		}
		if _, err := s.authenticateMember(ctx, tx, id, memberToken); err != nil {
			return err
		}
		var err error
		if info, err = sessionInfo(ctx, tx, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return team.SessionInfo{}, err
	}
	if err := s.truncateWAL(ctx); err != nil {
		log.Printf("session=%s stopped but WAL was not truncated, so its data may stay in the WAL file until the next checkpoint: %v", id, err)
	}
	return info, nil
}

func (s *Store) truncateWAL(ctx context.Context) error {
	var busy, walFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("checkpoint was blocked by another connection")
	}
	return nil
}

func (s *Store) Join(ctx context.Context, id, sessionToken, name, mcpVersion, presentedToken string) (team.JoinResponse, error) {
	var response team.JoinResponse
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireSessionToken(ctx, tx, id, sessionToken); err != nil {
			return err
		}
		var err error
		response, err = s.joinMember(ctx, tx, id, name, mcpVersion, presentedToken)
		return err
	})
	return response, err
}

func (s *Store) joinMember(ctx context.Context, tx *sql.Tx, id, name, mcpVersion, presentedToken string) (team.JoinResponse, error) {
	var storedHash string
	err := tx.QueryRowContext(ctx, `SELECT secret_hash FROM members WHERE session_id = ? AND name = ?`, id, name).Scan(&storedHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return team.JoinResponse{}, err
	case storedHash == "" || presentedToken == "" || hashSecret(presentedToken) != storedHash:
		return team.JoinResponse{}, ErrNameTaken
	}

	memberToken, err := newSecret()
	if err != nil {
		return team.JoinResponse{}, err
	}
	now := s.timestamp()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO members (session_id, name, mcp_version, secret_hash, joined_at, last_seen) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (session_id, name) DO UPDATE SET mcp_version = excluded.mcp_version, secret_hash = excluded.secret_hash, last_seen = excluded.last_seen`,
		id, name, mcpVersion, hashSecret(memberToken), now, now); err != nil {
		return team.JoinResponse{}, err
	}
	joined, err := appendMessage(ctx, tx, id, name, "join", name+" joined the session", now)
	if err != nil {
		return team.JoinResponse{}, err
	}
	info, err := sessionInfo(ctx, tx, id)
	if err != nil {
		return team.JoinResponse{}, err
	}
	return team.JoinResponse{Session: info, JoinID: joined.ID, MemberToken: memberToken}, nil
}

func (s *Store) Append(ctx context.Context, id, memberToken, kind, text string) (team.Message, error) {
	var message team.Message
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireSession(ctx, tx, id); err != nil {
			return err
		}
		from, err := s.authenticateMember(ctx, tx, id, memberToken)
		if err != nil {
			return err
		}
		message, err = appendMessage(ctx, tx, id, from, kind, text, s.timestamp())
		return err
	})
	return message, err
}

func (s *Store) Since(ctx context.Context, id, memberToken string, since int64, limit int) (team.MessagesResponse, error) {
	var response team.MessagesResponse
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var latestID int64
		err := tx.QueryRowContext(ctx, `SELECT latest_id FROM sessions WHERE id = ?`, id).Scan(&latestID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		if _, err := s.authenticateMember(ctx, tx, id, memberToken); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT id, sender, kind, text, created_at FROM messages
			 WHERE session_id = ? AND id > ? ORDER BY id LIMIT ?`,
			id, max(since, 0), limit)
		if err != nil {
			return err
		}
		defer rows.Close()

		messages := []team.Message{}
		for rows.Next() {
			message := team.Message{SessionID: id}
			var createdAt int64
			if err := rows.Scan(&message.ID, &message.From, &message.Kind, &message.Text, &createdAt); err != nil {
				return err
			}
			message.CreatedAt = fromTimestamp(createdAt)
			messages = append(messages, message)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		lastReturned := max(since, 0)
		if len(messages) > 0 {
			lastReturned = messages[len(messages)-1].ID
		}
		response = team.MessagesResponse{Messages: messages, LatestID: latestID, HasMore: lastReturned < latestID}
		return nil
	})
	return response, err
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) timestamp() int64 {
	return s.now().UTC().UnixNano()
}

func fromTimestamp(nanos int64) time.Time {
	return time.Unix(0, nanos).UTC()
}

func requireSessionToken(ctx context.Context, tx *sql.Tx, id, sessionToken string) error {
	var storedHash string
	err := tx.QueryRowContext(ctx, `SELECT token_hash FROM sessions WHERE id = ?`, id).Scan(&storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	if storedHash == "" || sessionToken == "" || hashSecret(sessionToken) != storedHash {
		return ErrBadSessionToken
	}
	return nil
}

func requireSession(ctx context.Context, tx *sql.Tx, id string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	return err
}

func (s *Store) authenticateMember(ctx context.Context, tx *sql.Tx, id, memberToken string) (string, error) {
	if memberToken == "" {
		return "", ErrNotMember
	}
	var name string
	err := tx.QueryRowContext(ctx,
		`UPDATE members SET last_seen = ? WHERE session_id = ? AND secret_hash = ? RETURNING name`,
		s.timestamp(), id, hashSecret(memberToken)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotMember
	}
	return name, err
}

func appendMessage(ctx context.Context, tx *sql.Tx, id, from, kind, text string, now int64) (team.Message, error) {
	var messageID int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE sessions SET latest_id = latest_id + 1 WHERE id = ? RETURNING latest_id`, id).Scan(&messageID); err != nil {
		return team.Message{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (session_id, id, sender, kind, text, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, from, kind, text, now); err != nil {
		return team.Message{}, err
	}
	return team.Message{
		ID:        messageID,
		SessionID: id,
		From:      from,
		Kind:      kind,
		Text:      text,
		CreatedAt: fromTimestamp(now),
	}, nil
}

func sessionInfo(ctx context.Context, tx *sql.Tx, id string) (team.SessionInfo, error) {
	info := team.SessionInfo{ID: id, Members: []team.Member{}}
	var createdAt int64
	err := tx.QueryRowContext(ctx,
		`SELECT name, created_by, created_at, latest_id FROM sessions WHERE id = ?`, id).
		Scan(&info.Name, &info.CreatedBy, &createdAt, &info.LatestID)
	if errors.Is(err, sql.ErrNoRows) {
		return team.SessionInfo{}, ErrSessionNotFound
	}
	if err != nil {
		return team.SessionInfo{}, err
	}
	info.CreatedAt = fromTimestamp(createdAt)

	rows, err := tx.QueryContext(ctx,
		`SELECT name, mcp_version, joined_at, last_seen FROM members WHERE session_id = ? ORDER BY joined_at, rowid`, id)
	if err != nil {
		return team.SessionInfo{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var member team.Member
		var joinedAt, lastSeen int64
		if err := rows.Scan(&member.Name, &member.MCPVersion, &joinedAt, &lastSeen); err != nil {
			return team.SessionInfo{}, err
		}
		member.JoinedAt = fromTimestamp(joinedAt)
		member.LastSeen = fromTimestamp(lastSeen)
		info.Members = append(info.Members, member)
	}
	return info, rows.Err()
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
