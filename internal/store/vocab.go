package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// VocabFile is one source file's contribution to the code vocabulary: its
// content hash and the names read from it (see package vocab). Only names
// are stored, never the file's text.
type VocabFile struct {
	Path, Hash string
	Names      []string
}

// VocabVersionKey is the meta key holding the vocab.Version the stored
// names were read with.
const VocabVersionKey = "vocab_version"

// VocabStored is what the index holds for one vocabulary file: its hash and
// how many names it gave.
type VocabStored struct {
	Hash  string
	Names int
}

// VocabFiles returns the stored hash and name count of every file the
// vocabulary was read from, by path.
func (s *Store) VocabFiles(ctx context.Context) (map[string]VocabStored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, hash, json_array_length(names) FROM vocab_files`)
	if err != nil {
		return nil, fmt.Errorf("store: vocabulary: %w", err)
	}
	defer rows.Close()
	out := map[string]VocabStored{}
	for rows.Next() {
		var p string
		var v VocabStored
		if err := rows.Scan(&p, &v.Hash, &v.Names); err != nil {
			return nil, err
		}
		out[p] = v
	}
	return out, rows.Err()
}

// ClearVocab empties the vocabulary tables and records version as the
// extractor the next names are read with, in one transaction. Only the
// writer calls it.
func (s *Store) ClearVocab(ctx context.Context, version string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM vocab; DELETE FROM vocab_files`); err != nil {
		return fmt.Errorf("store: clearing vocabulary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, VocabVersionKey, version); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateVocab replaces the rows of the files in put, removes those of the
// paths in del, and rebuilds the vocab table from every file's names, all
// in one transaction. With nothing to change it does nothing. Only the
// writer calls it.
func (s *Store) UpdateVocab(ctx context.Context, put []VocabFile, del []string) error {
	if len(put) == 0 && len(del) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range del {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vocab_files WHERE path = ?`, p); err != nil {
			return fmt.Errorf("store: vocabulary: %w", err)
		}
	}
	for _, f := range put {
		names := f.Names
		if names == nil {
			names = []string{}
		}
		js, err := json.Marshal(names)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vocab_files(path, hash, names) VALUES(?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET hash = excluded.hash, names = excluded.names`, f.Path, f.Hash, string(js)); err != nil {
			return fmt.Errorf("store: vocabulary: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vocab;
		INSERT OR IGNORE INTO vocab(name) SELECT j.value FROM vocab_files, json_each(vocab_files.names) AS j
			WHERE j.type = 'text'`); err != nil {
		return fmt.Errorf("store: rebuilding vocabulary: %w", err)
	}
	return tx.Commit()
}

// InVocab reports whether any of words (already lowercased) is in the code
// vocabulary: one lookup on the table's primary key.
func (s *Store) InVocab(ctx context.Context, words []string) (bool, error) {
	if len(words) == 0 {
		return false, nil
	}
	args := make([]any, len(words))
	for i, w := range words {
		args[i] = w
	}
	q := `SELECT 1 FROM vocab WHERE name IN (?` + strings.Repeat(`, ?`, len(words)-1) + `) LIMIT 1`
	var one int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// VocabStats returns the vocabulary's size, the number of files it was read
// from, and up to sample of its names in order.
func (s *Store) VocabStats(ctx context.Context, sample int) (names, files int, some []string, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM vocab), (SELECT count(*) FROM vocab_files)`).
		Scan(&names, &files); err != nil {
		return 0, 0, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM vocab ORDER BY name LIMIT ?`, sample)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return 0, 0, nil, err
		}
		some = append(some, n)
	}
	return names, files, some, rows.Err()
}
