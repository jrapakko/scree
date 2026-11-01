# Plan: index for keyword search without an API key

Found 2026-11-01 while testing the registry image: with no API key, every file fails
to sync (`embed: no API key`), so a fresh index has no chunks and the `auto → keyword`
fallback from 0.3.3 finds nothing.

1. **Sync without a key stores chunks with no vectors.** When the embedder has no key
   (`HasKey()` false; not on API errors or outages, which keep today's retry and backoff),
   chunk the file and store its chunks, line ranges and text hashes as usual. The FTS
   triggers then fill the keyword index. Skip the vector inserts.
2. **Those files get embedded once a key appears.** Record that the file's chunks have no
   vectors (a column or meta flag, whichever fits the schema with the least change; a
   schema change needs the in-place upgrade path described under How indexing works). The
   next sync with a key embeds exactly those files, even though their content hash is
   unchanged. Never re-chunk or re-embed files that already have vectors.
3. **Search.** Semantic search ignores chunks without vectors (check vec1 handles missing
   rows; a join should). Hybrid works with whatever vectors exist.
4. **Reporting.** `doctor` and the `[index incomplete …]`-style notes say how many files
   lack vectors ("12 files indexed for keyword search only; run scree key").
   `reindex` output counts them as indexed (keyword only) rather than failed. The log line
   per file is replaced by one summary line.
5. **Tests.** No key: sync indexes, keyword search finds the text, auto falls back to
   keyword and finds it. Then with a fake key: the next sync embeds those files only, and
   semantic search finds them. An API error with a key still counts as failed and is
   retried as today. Upgrade of an existing index keeps working.
6. Docs: SPEC How indexing works and Modes; README Search modes / settings.
