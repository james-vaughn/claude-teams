package relay

import (
	"context"
	"crypto/rand"
	"database/sql"
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
	ErrTooManySessions = errors.New("server has reached its session limit")
	ErrNotMember       = errors.New("join the session before publishing")
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	latest_id  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS members (
	session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	mcp_version TEXT NOT NULL DEFAULT '',
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

const tokenKey = "token"

type Store struct {
	db          *sql.DB
	maxSessions int
	now         func() time.Time
}

func OpenStore(ctx context.Context, path string, maxSessions int) (*Store, error) {
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
	if err := addMemberVersionColumn(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema in %s: %w", path, err)
	}
	return &Store{db: db, maxSessions: maxSessions, now: time.Now}, nil
}

func addMemberVersionColumn(ctx context.Context, db *sql.DB) error {
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('members') WHERE name = 'mcp_version'`).Scan(&present); err != nil {
		return err
	}
	if present > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `ALTER TABLE members ADD COLUMN mcp_version TEXT NOT NULL DEFAULT ''`)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Token(ctx context.Context) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, tokenKey).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return token, err
}

func (s *Store) SetToken(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		tokenKey, token)
	return err
}

func (s *Store) Create(ctx context.Context, name, createdBy string) (team.SessionInfo, error) {
	id, err := newUUID()
	if err != nil {
		return team.SessionInfo{}, err
	}

	var info team.SessionInfo
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
			return err
		}
		if count >= s.maxSessions {
			return ErrTooManySessions
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, name, created_by, created_at) VALUES (?, ?, ?, ?)`,
			id, name, createdBy, s.timestamp()); err != nil {
			return err
		}
		info, err = sessionInfo(ctx, tx, id)
		return err
	})
	return info, err
}

func (s *Store) Info(ctx context.Context, id string) (team.SessionInfo, error) {
	var info team.SessionInfo
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		info, err = sessionInfo(ctx, tx, id)
		return err
	})
	return info, err
}

func (s *Store) Stop(ctx context.Context, id string) (team.SessionInfo, error) {
	var info team.SessionInfo
	err := s.inTx(ctx, func(tx *sql.Tx) error {
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

func (s *Store) Join(ctx context.Context, id, name, mcpVersion string) (team.JoinResponse, error) {
	var response team.JoinResponse
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireSession(ctx, tx, id); err != nil {
			return err
		}
		now := s.timestamp()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO members (session_id, name, mcp_version, joined_at, last_seen) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (session_id, name) DO UPDATE SET mcp_version = excluded.mcp_version, last_seen = excluded.last_seen`,
			id, name, mcpVersion, now, now); err != nil {
			return err
		}
		joined, err := appendMessage(ctx, tx, id, name, "join", name+" joined the session", now)
		if err != nil {
			return err
		}
		info, err := sessionInfo(ctx, tx, id)
		if err != nil {
			return err
		}
		response = team.JoinResponse{Session: info, JoinID: joined.ID}
		return nil
	})
	return response, err
}

func (s *Store) Append(ctx context.Context, id, from, kind, text string) (team.Message, error) {
	var message team.Message
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := requireSession(ctx, tx, id); err != nil {
			return err
		}
		now := s.timestamp()
		touched, err := touchMember(ctx, tx, id, from, now)
		if err != nil {
			return err
		}
		if !touched {
			return ErrNotMember
		}
		message, err = appendMessage(ctx, tx, id, from, kind, text, now)
		return err
	})
	return message, err
}

func (s *Store) Since(ctx context.Context, id, memberName string, since int64, limit int) (team.MessagesResponse, error) {
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
		if memberName != "" {
			if _, err := touchMember(ctx, tx, id, memberName, s.timestamp()); err != nil {
				return err
			}
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

func requireSession(ctx context.Context, tx *sql.Tx, id string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	return err
}

func touchMember(ctx context.Context, tx *sql.Tx, id, name string, now int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE members SET last_seen = ? WHERE session_id = ? AND name = ?`, now, id, name)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
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
