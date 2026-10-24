package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// InVocabAndNotes reports whether any of words (already lowercased) is in
// the code vocabulary and also occurs in the notes, as keyword search would
// match it: one primary-key lookup, then one FTS5 query over the names
// found, each as a quoted phrase. The phrase is tokenized as the notes
// were, so pagefetcher matches PageFetcher, and golang.org/x/sync matches
// the same tokens in a row. A name only in code never counts.
func (s *Store) InVocabAndNotes(ctx context.Context, words []string) (bool, error) {
	if len(words) == 0 {
		return false, nil
	}
	args := make([]any, len(words))
	for i, w := range words {
		args[i] = w
	}
	found, err := s.column(ctx, `SELECT name FROM vocab WHERE name IN (?`+strings.Repeat(`, ?`, len(words)-1)+`)`, args...)
	if err != nil || len(found) == 0 {
		return false, err
	}
	for i, n := range found {
		found[i] = phrase(n)
	}
	return s.inNotes(ctx, strings.Join(found, " OR "))
}

// phrase quotes name as an FTS5 phrase, with inner quotes doubled.
func phrase(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// inNotes reports whether the FTS5 query match finds any chunk.
func (s *Store) inNotes(ctx context.Context, match string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM chunks_fts WHERE chunks_fts MATCH ? LIMIT 1`, match).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// VocabStats returns the vocabulary's size, how many of its names also
// occur in the notes (see InVocabAndNotes), the number of files it was read
// from, and up to sample of the names that occur in the notes, in order.
// No name found only in code is returned. One-token names are matched
// against the keyword index's terms in one join; the others, such as
// dotted module paths, by one phrase query each.
func (s *Store) VocabStats(ctx context.Context, sample int) (names, inNotes, files int, some []string, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM vocab), (SELECT count(*) FROM vocab_files)`).
		Scan(&names, &files); err != nil {
		return 0, 0, 0, nil, err
	}
	found, err := s.column(ctx, `SELECT v.name FROM chunks_terms t JOIN vocab v ON v.name = t.term`)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	// Names with a character the tokenizer splits on: GLOB's [^-_a-z0-9]
	// is any other character.
	multi, err := s.column(ctx, `SELECT name FROM vocab WHERE name GLOB '*[^-_a-z0-9]*'`)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	for _, n := range multi {
		ok, err := s.inNotes(ctx, phrase(n))
		if err != nil {
			return 0, 0, 0, nil, err
		}
		if ok {
			found = append(found, n)
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	return names, len(found), files, found[:min(sample, len(found))], nil
}

// VocabNames returns up to n of the vocabulary's names in order, whether or
// not the notes hold them. It is for tests; nothing should print its
// result, since a name only in code may be a misread string.
func (s *Store) VocabNames(ctx context.Context, n int) ([]string, error) {
	return s.column(ctx, `SELECT name FROM vocab ORDER BY name LIMIT ?`, n)
}

// column returns the one text column of query's rows.
func (s *Store) column(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
