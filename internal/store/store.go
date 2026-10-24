// Package store keeps chunks and their vectors in SQLite, using the vec1
// extension for nearest-neighbor search.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/vec1"
)

// ErrModelMismatch means the index was built with a different embedding
// model than the one configured. Rebuild with `scree reindex --all`.
var ErrModelMismatch = errors.New("index was built with a different embedding model; run `scree reindex --all`")

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS files(
	path TEXT PRIMARY KEY,
	hash TEXT NOT NULL,
	indexed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chunks(
	id INTEGER PRIMARY KEY,
	path TEXT NOT NULL,
	ord INTEGER NOT NULL,
	heading TEXT NOT NULL,
	line INTEGER NOT NULL,
	text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS chunks_path ON chunks(path);
`

// Store is an open index database.
type Store struct {
	db   *sql.DB
	path string
	fi   os.FileInfo // the database file as opened, for CheckFile
}

// ErrIndexGone means the index file was deleted or replaced while open.
var ErrIndexGone = errors.New("index file was removed or replaced")

// Chunk is a stored piece of a note.
type Chunk struct {
	Path    string
	Heading string
	Line    int
	Text    string
}

// Hit is a search result.
type Hit struct {
	Chunk
	// Distance is the cosine distance to the query: 0 is identical, 2 is opposite.
	Distance float64
}

// Open opens or creates the index at path. A missing parent folder is
// created with mode 0700; an existing one is left alone. The database and
// its -wal and -shm files are made 0600.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(wal)&_pragma=busy_timeout(10000)&_pragma=synchronous(normal)"
	db, err := driver.Open(dsn, vec1.Register)
	if err != nil {
		return nil, err
	}
	// Each connection is a separate SQLite instance; one keeps memory flat.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	if s.fi, err = os.Stat(path); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return s, nil
}

// CheckFile returns ErrIndexGone if the file at the index path is no longer
// the one that was opened.
func (s *Store) CheckFile() error {
	fi, err := os.Stat(s.path)
	if err != nil || !os.SameFile(fi, s.fi) {
		return fmt.Errorf("%w: %s", ErrIndexGone, s.path)
	}
	return nil
}

func (s *Store) init() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: schema: %w", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'vecs'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	// A flat index gives exact search, which is fast enough at notes scale and
	// needs no training step.
	_, err := s.db.Exec(`
		CREATE VIRTUAL TABLE vecs USING vec1(vector);
		INSERT INTO vecs(cmd, vector) VALUES('rebuild', '{index:"flat", distance:"cos"}');`)
	if err != nil {
		return fmt.Errorf("store: creating vector table: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// CheckModel records model on first use and returns ErrModelMismatch if the
// index already holds vectors from a different one.
func (s *Store) CheckModel(model string) error {
	var have string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'model'`).Scan(&have)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO meta(key, value) VALUES('model', ?)`, model)
		return err
	case err != nil:
		return err
	case have != model:
		return fmt.Errorf("%w (index: %s, configured: %s)", ErrModelMismatch, have, model)
	}
	return nil
}

// ErrRootMismatch means the index was built for a different folder. Syncing
// it against this one would delete the other folder's rows.
var ErrRootMismatch = errors.New("index root mismatch")

// Meta returns the meta value for key, or "" if it is not set.
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta sets the meta value for key, replacing any earlier one.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// CheckRoot records root, the indexed folder with symlinks resolved, on
// first use and returns ErrRootMismatch if the index was built for another
// folder. An index with files but no recorded root (from the single-folder
// version) is adopted for root; the next sync reconciles it.
func (s *Store) CheckRoot(root string) error {
	have, err := s.Meta("root")
	switch {
	case err != nil:
		return err
	case have == "":
		return s.SetMeta("root", root)
	case have != root:
		return fmt.Errorf("%w: index %s was built for %s, not %s; pass a different -db or delete it",
			ErrRootMismatch, s.path, have, root)
	}
	return nil
}

// Model returns the model the index was built with, or "" if none yet.
func (s *Store) Model() (string, error) {
	var m string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'model'`).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m, err
}

// FileHash returns the stored content hash for path, or "" if not indexed.
func (s *Store) FileHash(ctx context.Context, path string) (string, error) {
	var h string
	err := s.db.QueryRowContext(ctx, `SELECT hash FROM files WHERE path = ?`, path).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

// Paths returns every indexed file path.
func (s *Store) Paths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplaceFile atomically swaps a file's chunks and vectors for new ones.
func (s *Store) ReplaceFile(ctx context.Context, path, hash string, unix int64, chunks []Chunk, vecs [][]float32) error {
	if len(chunks) != len(vecs) {
		return fmt.Errorf("store: %d chunks but %d vectors", len(chunks), len(vecs))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteFile(ctx, tx, path); err != nil {
		return err
	}
	for i, c := range chunks {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO chunks(path, ord, heading, line, text) VALUES(?, ?, ?, ?, ?)`,
			path, i, c.Heading, c.Line, c.Text)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vecs(rowid, vector) VALUES(?, ?)`, id, encode(vecs[i])); err != nil {
			return fmt.Errorf("store: inserting vector: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files(path, hash, indexed_at) VALUES(?, ?, ?)`, path, hash, unix); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveFile deletes a file's rows. Removing an unknown path is not an error.
func (s *Store) RemoveFile(ctx context.Context, path string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteFile(ctx, tx, path); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteFile(ctx context.Context, tx *sql.Tx, path string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vecs WHERE rowid IN (SELECT id FROM chunks WHERE path = ?)`, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE path = ?`, path); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path)
	return err
}

// Search returns the k chunks nearest to query.
func (s *Store) Search(ctx context.Context, query []float32, k int) ([]Hit, error) {
	// vec1 learns the vector size from the first insert, so an empty index
	// rejects queries outright.
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.path, c.heading, c.line, c.text, v.distance
		FROM (SELECT rowid, distance FROM vecs WHERE cmd = ? LIMIT ?) AS v
		JOIN chunks c ON c.id = v.rowid
		ORDER BY v.distance`, encode(query), k)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Path, &h.Heading, &h.Line, &h.Text, &h.Distance); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// Stats summarizes the index.
type Stats struct {
	Files, Chunks int
	Vec1Info      string
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM files), (SELECT count(*) FROM chunks), vec1_info()`).
		Scan(&st.Files, &st.Chunks, &st.Vec1Info)
	return st, err
}

// ChunkCount returns the number of chunks in the index.
func (s *Store) ChunkCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks`).Scan(&n)
	return n, err
}

// Reset deletes everything, including the recorded model.
func (s *Store) Reset(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DROP TABLE IF EXISTS vecs;
		DELETE FROM chunks;
		DELETE FROM files;
		DELETE FROM meta;`)
	if err != nil {
		return err
	}
	return s.init()
}

func encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}
