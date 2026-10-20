package storage

import (
	"database/sql"
	"fmt"
)

const CurrentSchemaVersion = 1

const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA max_page_count = 8192;

CREATE TABLE IF NOT EXISTS messages (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    pid        TEXT,             -- Poster ID; NULL once tombstoned
    nick       TEXT,             -- Display nick (or nick!trip); NULL once tombstoned
    body       BLOB,             -- Message text; NULL once tombstoned
    nonce      BLOB,             -- Per-message random salt; NULL once tombstoned
    created_at INTEGER NOT NULL, -- Unix millisecond timestamp
    leaf       BLOB NOT NULL     -- 32-byte cryptographic leaf hash (retained permanently)
);

CREATE TABLE IF NOT EXISTS blocks (
    idx       INTEGER PRIMARY KEY, -- seq >> 7
    first_seq INTEGER NOT NULL,
    count     INTEGER NOT NULL,
    root      BLOB NOT NULL,       -- Merkle root of the block's leaves
    prev      BLOB NOT NULL,       -- Hash of previous block header
    sig       BLOB NOT NULL        -- Server signature over current block header
);

CREATE TABLE IF NOT EXISTS meta (
    k TEXT PRIMARY KEY,
    v BLOB
);
`

// InitSchema sets pragmas, creates tables, and runs version check/migration.
func InitSchema(db *sql.DB) error {
	// Set PRAGMAs individually to ensure they execute properly
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA max_page_count = 8192;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("failed pragma %s: %w", p, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("failed executing schema: %w", err)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version;").Scan(&version); err != nil {
		return fmt.Errorf("failed reading user_version: %w", err)
	}

	if version == 0 {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d;", CurrentSchemaVersion)); err != nil {
			return fmt.Errorf("failed setting user_version: %w", err)
		}
		// Initialize meta defaults if not present
		initMetaSQL := `
		INSERT OR IGNORE INTO meta (k, v) VALUES ('chain_anchor', X'0000000000000000000000000000000000000000000000000000000000000000');
		INSERT OR IGNORE INTO meta (k, v) VALUES ('total_bytes', 0);
		INSERT OR IGNORE INTO meta (k, v) VALUES ('epoch', '1');
		`
		if _, err := db.Exec(initMetaSQL); err != nil {
			return fmt.Errorf("failed initializing meta: %w", err)
		}
	}

	return nil
}
