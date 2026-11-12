// Package cache is OctoGuard's on-disk cache: response entries (body, ETag,
// Last-Modified) and hourly usage counters, stored in SQLite (WAL) and shared
// by every process on the machine.
//
// Every row is scoped by an opaque tokenID (host + token hash), so entries
// fetched with one token are never visible to another.
package cache

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no cgo)
	sqlite3 "modernc.org/sqlite/lib"
)

// Entry is a cached GitHub response.
type Entry struct {
	Body         []byte
	ETag         string
	LastModified string
	FetchedAt    time.Time
	Via          string // "rest" or "graphql"
	Cost         int64  // GraphQL points the entry cost to fetch; 0 for REST
}

// Bucket is the total of one counter within one hour.
type Bucket struct {
	Name string
	Hour time.Time // start of the hour, UTC
	N    int64
}

// Cache is a handle on the SQLite cache file. It is safe for concurrent use,
// and several handles (in one or many processes) may share the same file.
type Cache struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	token_id      TEXT    NOT NULL,
	key           TEXT    NOT NULL,
	body          BLOB    NOT NULL,
	etag          TEXT    NOT NULL,
	last_modified TEXT    NOT NULL,
	fetched_at    INTEGER NOT NULL, -- unix nanoseconds
	via           TEXT    NOT NULL,
	cost          INTEGER NOT NULL DEFAULT 0, -- GraphQL points spent to fetch it
	PRIMARY KEY (token_id, key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS counters (
	token_id TEXT    NOT NULL,
	name     TEXT    NOT NULL,
	hour     INTEGER NOT NULL, -- unix seconds, truncated to the hour
	n        INTEGER NOT NULL,
	PRIMARY KEY (token_id, name, hour)
) WITHOUT ROWID;`

// Open opens (creating it if needed) the cache database at path. The parent
// directory is created with mode 0700 and the file with mode 0600.
func Open(path string) (*Cache, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("cache: create dir: %w", err)
	}
	// Create the file ourselves so it never exists with a wider mode. SQLite
	// gives the -wal and -shm files the same mode as the database file.
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cache: create file: %w", err)
	}
	_ = f.Close()
	if err := os.Chmod(abs, 0o600); err != nil {
		return nil, fmt.Errorf("cache: chmod: %w", err)
	}

	// busy_timeout makes concurrent writers wait instead of failing with
	// SQLITE_BUSY; _txlock=immediate takes the write lock at BEGIN.
	dsn := "file:" + (&url.URL{Path: abs}).EscapedPath() +
		"?_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: open: %w", err)
	}
	if err := enableWAL(db, busyTimeout); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: enable WAL: %w", err)
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: init schema: %w", err)
	}
	return &Cache{db: db}, nil
}

const busyTimeout = 10 * time.Second

// enableWAL switches the file to WAL mode, which is persistent. SQLite returns
// SQLITE_BUSY from this pragma without consulting busy_timeout when another
// connection holds a lock on the file, so the busy wait is done here.
func enableWAL(db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var mode string
		err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil {
			if !strings.EqualFold(mode, "wal") {
				return fmt.Errorf("journal_mode is %q", mode)
			}
			return nil
		}
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// migrations bring a database created by an older version up to schema.
// Each one runs only when its column is missing.
var migrations = []struct{ table, column, ddl string }{
	{"entries", "cost", `ALTER TABLE entries ADD COLUMN cost INTEGER NOT NULL DEFAULT 0`},
}

// initSchema creates the tables, and adds the columns an older database
// lacks, inside a BEGIN IMMEDIATE transaction
// (_txlock=immediate): the write lock is taken up front, under busy_timeout,
// instead of being upgraded from a read lock, which SQLite fails immediately
// with SQLITE_BUSY when another process is initialising the same file.
func initSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(schema); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, m := range migrations {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			_ = tx.Rollback()
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec(m.ddl); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Close releases the database handle.
func (c *Cache) Close() error { return c.db.Close() }

// Get returns the entry for key under tokenID. ok is false if there is none.
func (c *Cache) Get(tokenID, key string) (e Entry, ok bool, err error) {
	var fetched int64
	err = c.db.QueryRow(
		`SELECT body, etag, last_modified, fetched_at, via, cost FROM entries WHERE token_id = ? AND key = ?`,
		tokenID, key,
	).Scan(&e.Body, &e.ETag, &e.LastModified, &fetched, &e.Via, &e.Cost)
	if err == sql.ErrNoRows {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache: get: %w", err)
	}
	e.FetchedAt = time.Unix(0, fetched).UTC()
	return e, true, nil
}

// Put stores e under key for tokenID, replacing any previous entry.
func (c *Cache) Put(tokenID, key string, e Entry) error {
	body := e.Body
	if body == nil {
		body = []byte{}
	}
	_, err := c.db.Exec(
		`INSERT INTO entries (token_id, key, body, etag, last_modified, fetched_at, via, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (token_id, key) DO UPDATE SET
		   body = excluded.body, etag = excluded.etag, last_modified = excluded.last_modified,
		   fetched_at = excluded.fetched_at, via = excluded.via, cost = excluded.cost`,
		tokenID, key, body, e.ETag, e.LastModified, e.FetchedAt.UnixNano(), e.Via, e.Cost,
	)
	if err != nil {
		return fmt.Errorf("cache: put: %w", err)
	}
	return nil
}

// Invalidate deletes the given keys for tokenID.
func (c *Cache) Invalidate(tokenID string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := make([]any, 0, len(keys)+1)
	args = append(args, tokenID)
	for _, k := range keys {
		args = append(args, k)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	if _, err := c.db.Exec(`DELETE FROM entries WHERE token_id = ? AND key IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("cache: invalidate: %w", err)
	}
	return nil
}

// InvalidatePrefix deletes every key of tokenID that starts with prefix.
// The match is literal and case-sensitive (no LIKE wildcards).
func (c *Cache) InvalidatePrefix(tokenID, prefix string) error {
	_, err := c.db.Exec(
		`DELETE FROM entries WHERE token_id = ? AND substr(key, 1, length(?)) = ?`,
		tokenID, prefix, prefix,
	)
	if err != nil {
		return fmt.Errorf("cache: invalidate prefix: %w", err)
	}
	return nil
}

// IncCounter adds n to counter name for tokenID in the hour containing at.
func (c *Cache) IncCounter(tokenID, name string, n int64, at time.Time) error {
	_, err := c.db.Exec(
		`INSERT INTO counters (token_id, name, hour, n) VALUES (?, ?, ?, ?)
		 ON CONFLICT (token_id, name, hour) DO UPDATE SET n = n + excluded.n`,
		tokenID, name, at.UTC().Truncate(time.Hour).Unix(), n,
	)
	if err != nil {
		return fmt.Errorf("cache: inc counter: %w", err)
	}
	return nil
}

// Counters returns the hourly buckets of tokenID whose hour starts at or after
// the hour containing since, ordered by hour and then name.
func (c *Cache) Counters(tokenID string, since time.Time) ([]Bucket, error) {
	rows, err := c.db.Query(
		`SELECT name, hour, n FROM counters WHERE token_id = ? AND hour >= ? ORDER BY hour, name`,
		tokenID, since.UTC().Truncate(time.Hour).Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("cache: counters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		var hour int64
		if err := rows.Scan(&b.Name, &hour, &b.N); err != nil {
			return nil, fmt.Errorf("cache: counters: %w", err)
		}
		b.Hour = time.Unix(hour, 0).UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cache: counters: %w", err)
	}
	return out, nil
}
