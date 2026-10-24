// Package store keeps chunks and their vectors in SQLite, using the vec1
// extension for nearest-neighbor search.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	indexed_at INTEGER NOT NULL,
	mtime INTEGER,
	size INTEGER
);
CREATE TABLE IF NOT EXISTS chunks(
	id INTEGER PRIMARY KEY,
	path TEXT NOT NULL,
	ord INTEGER NOT NULL,
	heading TEXT NOT NULL,
	line INTEGER NOT NULL,
	text TEXT NOT NULL,
	end_line INTEGER,
	text_hash TEXT
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

// SchemaVersion is recorded as meta "schema" once the tables have every
// column this version uses.
const SchemaVersion = "2"

// Chunk is a stored piece of a note.
type Chunk struct {
	Path    string
	Heading string
	// Line and EndLine are the first and last non-blank source lines of the
	// chunk. EndLine is 0 for rows from before it was recorded.
	Line, EndLine int
	Text          string
}

// Copy is another chunk with the same text as a hit, at its stored range.
type Copy struct {
	Path          string
	Line, EndLine int
}

// TextHash is the stored hash of a chunk's text: the first 16 bytes of its
// SHA-256, in hex. Identical chunks in several files share it.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// LineRange is a chunk's source line range, by its order in the file.
type LineRange struct {
	Line, EndLine int
}

// FileStat is the modification time (Unix nanoseconds) and size of a file's
// content as it was indexed.
type FileStat struct {
	Mtime, Size int64
}

// Hit is a search result.
type Hit struct {
	Chunk
	ID       int64  // the chunk's row id
	TextHash string // "" for rows from before it was recorded
	// Stale is set by search when the file changed since it was indexed and
	// the chunk's text is no longer in it; Line and EndLine are then 0.
	Stale bool
	// Also lists other files holding the same chunk, and AlsoMore how many
	// more there are, when search folded duplicates into this hit.
	Also     []Copy
	AlsoMore int
	// Stat is the file as it was indexed; nil for rows from before it was
	// recorded.
	Stat *FileStat
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
		"?_pragma=busy_timeout(10000)&_pragma=synchronous(normal)"
	db, err := driver.Open(dsn, vec1.Register)
	if err != nil {
		return nil, err
	}
	// Each connection is a separate SQLite instance; one keeps memory flat.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := setWAL(db); err != nil {
		db.Close()
		return nil, err
	}
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

// setWAL switches the database to WAL mode, which is recorded in the file.
// Switching does not wait on busy_timeout, so when two processes open a fresh
// index at once the loser retries for up to 10 s.
func setWAL(db *sql.DB) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var mode string
		err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil && strings.EqualFold(mode, "wal") {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("journal mode is %s", mode)
		}
		if (!strings.Contains(err.Error(), "locked") && !strings.Contains(err.Error(), "busy")) || time.Now().After(deadline) {
			return fmt.Errorf("store: setting WAL mode: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// init creates the schema and the vector table if missing. It runs in one
// BEGIN IMMEDIATE transaction on one connection, so a second process opening
// a fresh index waits on busy_timeout and then sees the finished tables
// instead of racing to create them.
func (s *Store) init() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: schema: %w", err)
	}
	if err := migrate(ctx, conn); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if err := createVecs(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	committed = true
	return nil
}

// createVecs creates the vector table unless it exists. If another process
// created it first (possible only if creation could not run inside the
// transaction), the "already exists" error is checked against sqlite_master
// and ignored.
func createVecs(ctx context.Context, conn *sql.Conn) error {
	exists := func() (bool, error) {
		var n int
		err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'vecs'`).Scan(&n)
		return n > 0, err
	}
	if ok, err := exists(); err != nil || ok {
		return err
	}
	// A flat index gives exact search, which is fast enough at notes scale and
	// needs no training step.
	_, err := conn.ExecContext(ctx, `
		CREATE VIRTUAL TABLE vecs USING vec1(vector);
		INSERT INTO vecs(cmd, vector) VALUES('rebuild', '{index:"flat", distance:"cos"}');`)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		if ok, xerr := exists(); xerr == nil && ok {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("store: creating vector table: %w", err)
	}
	return nil
}

// migrate brings an index from an older version up to SchemaVersion. The
// columns it adds are NULL in existing rows, which callers treat as
// unknown: files.mtime and files.size (search re-reads the file), and
// chunks.end_line (filled in by the next sync without re-embedding).
func migrate(ctx context.Context, conn *sql.Conn) error {
	for _, add := range []struct{ table, col string }{
		{"files", "mtime"}, {"files", "size"}, {"chunks", "end_line"},
	} {
		if err := addColumn(ctx, conn, add.table, add.col, "INTEGER"); err != nil {
			return err
		}
	}
	if err := addColumn(ctx, conn, "chunks", "text_hash", "TEXT"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS chunks_text_hash ON chunks(text_hash)`); err != nil {
		return err
	}
	if err := backfillTextHash(ctx, conn); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('schema', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value WHERE value <> excluded.value`, SchemaVersion)
	return err
}

func addColumn(ctx context.Context, conn *sql.Conn, table, col, typ string) error {
	ok, err := hasColumn(ctx, conn, table, col)
	if err != nil || ok {
		return err
	}
	_, err = conn.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+col+` `+typ)
	return err
}

// backfillTextHash hashes the stored text of chunks from before text_hash
// was recorded, 1000 rows at a time. It runs once, inside init's
// transaction; nothing is re-embedded.
func backfillTextHash(ctx context.Context, conn *sql.Conn) error {
	for {
		rows, err := conn.QueryContext(ctx, `SELECT id, text FROM chunks WHERE text_hash IS NULL LIMIT 1000`)
		if err != nil {
			return err
		}
		type row struct {
			id   int64
			hash string
		}
		var batch []row
		for rows.Next() {
			var r row
			var text string
			if err := rows.Scan(&r.id, &text); err != nil {
				rows.Close()
				return err
			}
			r.hash = TextHash(text)
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, r := range batch {
			if _, err := conn.ExecContext(ctx, `UPDATE chunks SET text_hash = ? WHERE id = ?`, r.hash, r.id); err != nil {
				return err
			}
		}
	}
}

func hasColumn(ctx context.Context, conn *sql.Conn, table, col string) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n)
	return n > 0, err
}

func (s *Store) Close() error { return s.db.Close() }

// CheckModel records model on first use and returns ErrModelMismatch if the
// index already holds vectors from a different one. Only the writer calls it;
// readers use MatchModel.
func (s *Store) CheckModel(model string) error {
	// INSERT OR IGNORE, then compare: two processes recording at once both
	// see the one value that won.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES('model', ?)`, model); err != nil {
		return err
	}
	return s.MatchModel(model)
}

// MatchModel returns ErrModelMismatch if the index records a model other
// than model. It never writes.
func (s *Store) MatchModel(model string) error {
	have, err := s.Model()
	if err != nil {
		return err
	}
	if have != "" && have != model {
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
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES('root', ?)`, root); err != nil {
		return err
	}
	return s.MatchRoot(root)
}

// MatchRoot returns ErrRootMismatch if the index records a folder other than
// root. An index with no recorded folder matches. It never writes.
func (s *Store) MatchRoot(root string) error {
	have, err := s.Meta("root")
	if err != nil {
		return err
	}
	if have != "" && have != root {
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

// FileInfo returns the stored hash and stat for path: "" and nil if it is not
// indexed, and a nil stat if none was recorded.
func (s *Store) FileInfo(ctx context.Context, path string) (string, *FileStat, error) {
	var h string
	var mtime, size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT hash, mtime, size FROM files WHERE path = ?`, path).Scan(&h, &mtime, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return h, statOf(mtime, size), nil
}

// SetFileStat records the stat of an indexed file whose content is
// unchanged, for example after a touch.
func (s *Store) SetFileStat(ctx context.Context, path string, st FileStat) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET mtime = ?, size = ? WHERE path = ?`, st.Mtime, st.Size, path)
	return err
}

func statOf(mtime, size sql.NullInt64) *FileStat {
	if !mtime.Valid || !size.Valid {
		return nil
	}
	return &FileStat{Mtime: mtime.Int64, Size: size.Int64}
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
func (s *Store) ReplaceFile(ctx context.Context, path, hash string, unix int64, st *FileStat, chunks []Chunk, vecs [][]float32) error {
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
			`INSERT INTO chunks(path, ord, heading, line, end_line, text, text_hash) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			path, i, c.Heading, c.Line, nullInt(c.EndLine), c.Text, TextHash(c.Text))
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
	var mtime, size sql.NullInt64
	if st != nil {
		mtime = sql.NullInt64{Int64: st.Mtime, Valid: true}
		size = sql.NullInt64{Int64: st.Size, Valid: true}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files(path, hash, indexed_at, mtime, size) VALUES(?, ?, ?, ?, ?)`, path, hash, unix, mtime, size); err != nil {
		return err
	}
	return tx.Commit()
}

func nullInt(n int) sql.NullInt64 { return sql.NullInt64{Int64: int64(n), Valid: n > 0} }

// MissingLines reports whether any chunk of path has no recorded end line.
func (s *Store) MissingLines(ctx context.Context, path string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE path = ? AND end_line IS NULL`, path).Scan(&n)
	return n > 0, err
}

// ChunkTexts returns the stored text of path's chunks, in file order.
func (s *Store) ChunkTexts(ctx context.Context, path string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text FROM chunks WHERE path = ? ORDER BY ord`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetLines records the line ranges of path's chunks, by order, without
// touching their text or vectors. Only the writer calls it.
func (s *Store) SetLines(ctx context.Context, path string, ranges []LineRange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, r := range ranges {
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET line = ?, end_line = ? WHERE path = ? AND ord = ?`,
			r.Line, nullInt(r.EndLine), path, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Copies returns up to limit other chunks whose text hash is hash, skipping
// the chunk excludeID, ordered by path and line.
func (s *Store) Copies(ctx context.Context, hash string, excludeID int64, limit int) ([]Copy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, line, end_line FROM chunks
		WHERE text_hash = ? AND id <> ? ORDER BY path, line LIMIT ?`, hash, excludeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Copy
	for rows.Next() {
		var c Copy
		var end sql.NullInt64
		if err := rows.Scan(&c.Path, &c.Line, &end); err != nil {
			return nil, err
		}
		c.EndLine = int(end.Int64)
		out = append(out, c)
	}
	return out, rows.Err()
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
		SELECT c.id, c.text_hash, c.path, c.heading, c.line, c.end_line, c.text, v.distance, f.mtime, f.size
		FROM (SELECT rowid, distance FROM vecs WHERE cmd = ? LIMIT ?) AS v
		JOIN chunks c ON c.id = v.rowid
		LEFT JOIN files f ON f.path = c.path
		ORDER BY v.distance`, encode(query), k)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var mtime, size, end sql.NullInt64
		var th sql.NullString
		if err := rows.Scan(&h.ID, &th, &h.Path, &h.Heading, &h.Line, &end, &h.Text, &h.Distance, &mtime, &size); err != nil {
			return nil, err
		}
		h.EndLine, h.TextHash = int(end.Int64), th.String
		h.Stat = statOf(mtime, size)
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
