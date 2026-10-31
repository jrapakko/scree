package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// TextMatch is a text that matched a word, by its index in the texts
// given, and the byte offset of the match's first token in it.
type TextMatch struct{ Row, At int }

// MatchTexts reports, for each word, which texts match it as keyword
// search would: each word is quoted as phrase() quotes it and matched in
// a private in-memory FTS5 table using the keyword index's tokenizer.
// texts must hold no \x01 or \x02 (callers pass textsafe.Strip output).
// A panic in SQLite, such as running out of its memory, is returned as an
// error.
func MatchTexts(ctx context.Context, words, texts []string) (out [][]TextMatch, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("store: match: %v", r)
		}
	}()
	out = make([][]TextMatch, len(words))
	if len(words) == 0 || len(texts) == 0 {
		return out, nil
	}
	db, err := driver.Open("file::memory:", fts5.Register)
	if err != nil {
		return nil, fmt.Errorf("store: match: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// One connection: each in-memory connection is its own database.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: match: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE VIRTUAL TABLE t USING fts5(text, tokenize="`+ftsTokenize+`")`); err != nil {
		return nil, fmt.Errorf("store: match: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: match: %w", err)
	}
	defer tx.Rollback()
	for i, t := range texts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO t(rowid, text) VALUES(?, ?)`, i+1, t); err != nil {
			return nil, fmt.Errorf("store: match: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: match: %w", err)
	}
	for i, w := range words {
		rows, err := conn.QueryContext(ctx,
			`SELECT rowid, highlight(t, 0, char(1), char(2)) FROM t WHERE t MATCH ? ORDER BY rowid`, phrase(w))
		if err != nil {
			return nil, fmt.Errorf("store: match: %w", err)
		}
		for rows.Next() {
			var row int
			var marked string
			if err := rows.Scan(&row, &marked); err != nil {
				rows.Close()
				return nil, err
			}
			out[i] = append(out[i], TextMatch{Row: row - 1, At: max(strings.IndexByte(marked, 1), 0)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: match: %w", err)
		}
	}
	return out, nil
}
