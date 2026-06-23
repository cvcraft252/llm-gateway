package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrKeyNotFound is returned by Lookup when no active key matches the provided
// full key string.
var ErrKeyNotFound = errors.New("api key not found")

// ErrKeyRevoked is returned by Lookup when the key hash matches but the key
// has been revoked.
var ErrKeyRevoked = errors.New("api key has been revoked")

// APIKey represents a stored API key record. The hash is never exposed
// through JSON responses — only key_id, name, status, and timestamps.
type APIKey struct {
	ID        int64          `json:"id"`
	KeyID     string         `json:"key_id"`
	Name      string         `json:"name"`
	Status    string         `json:"status"`
	CreatedAt string         `json:"created_at"`
	RevokedAt sql.NullString `json:"revoked_at"`
}

// KeyStore manages API key persistence backed by the same *sql.DB as the
// audit log store. All methods are safe for concurrent use because
// database/sql manages connection pooling and SQLite WAL mode allows
// concurrent readers with a single writer.
type KeyStore struct {
	db *sql.DB
}

// NewKeyStore creates a KeyStore over the given database connection.
func NewKeyStore(database *DB) *KeyStore {
	return &KeyStore{db: database.Conn()}
}

// keyPrefix is the human-readable prefix that distinguishes gateway keys
// from other bearer tokens. The format is: gw- + 32 hex chars = 35 total.
const keyPrefix = "gw-"

// keyIDLength is the number of characters from the full key used as the
// lookup index. The first 12 chars (gw- + 9 hex) uniquely identify a key
// in the database without exposing the full key value.
const keyIDLength = 12

// Create generates a new API key, stores its SHA-256 hash, and returns the
// full key string. The full key is shown only once — callers must store it
// securely because it cannot be recovered from the hash.
//
// Key format: gw- + 32 hex chars from crypto/rand (128 bits of entropy).
// We use SHA-256 without salt because API keys are high-entropy random
// values — unlike human passwords, brute-forcing a 128-bit key space is
// computationally infeasible, and the fast hash keeps per-request auth
// latency under 1μs.
func (ks *KeyStore) Create(ctx context.Context, name string) (string, error) {
	rawBytes := make([]byte, 16)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	fullKey := keyPrefix + hex.EncodeToString(rawBytes)
	keyID := fullKey[:keyIDLength]
	keyHash := hashKey(fullKey)

	_, err := ks.db.ExecContext(ctx,
		`INSERT INTO api_keys (key_id, key_hash, name) VALUES (?, ?, ?)`,
		keyID, keyHash, name,
	)
	if err != nil {
		return "", fmt.Errorf("insert api key: %w", err)
	}

	return fullKey, nil
}

// Lookup validates an incoming full API key string. It extracts the key_id
// (first 12 chars) for a database lookup, then verifies the SHA-256 hash.
// Returns:
//   - *APIKey, nil: key is valid and active
//   - nil, ErrKeyRevoked: key hash matches but status is 'revoked'
//   - nil, ErrKeyNotFound: no key with matching key_id and hash
func (ks *KeyStore) Lookup(ctx context.Context, fullKey string) (*APIKey, error) {
	if len(fullKey) < keyIDLength {
		return nil, ErrKeyNotFound
	}

	keyID := fullKey[:keyIDLength]
	keyHash := hashKey(fullKey)

	var ak APIKey
	err := ks.db.QueryRowContext(ctx,
		`SELECT id, key_id, name, status, created_at, revoked_at
		 FROM api_keys
		 WHERE key_id = ? AND key_hash = ?`,
		keyID, keyHash,
	).Scan(&ak.ID, &ak.KeyID, &ak.Name, &ak.Status, &ak.CreatedAt, &ak.RevokedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query api key: %w", err)
	}

	if ak.Status == "revoked" {
		return nil, ErrKeyRevoked
	}

	return &ak, nil
}

// List returns all API keys, optionally filtered by status ("active",
// "revoked", or empty for all). Hashes are never included in the result.
func (ks *KeyStore) List(ctx context.Context, status string) ([]APIKey, error) {
	var (
		rows *sql.Rows
		err  error
	)

	if status == "" {
		rows, err = ks.db.QueryContext(ctx,
			`SELECT id, key_id, name, status, created_at, revoked_at
			 FROM api_keys ORDER BY id`)
	} else {
		rows, err = ks.db.QueryContext(ctx,
			`SELECT id, key_id, name, status, created_at, revoked_at
			 FROM api_keys WHERE status = ? ORDER BY id`, status)
	}
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var ak APIKey
		if err := rows.Scan(&ak.ID, &ak.KeyID, &ak.Name, &ak.Status, &ak.CreatedAt, &ak.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan api key row: %w", err)
		}
		keys = append(keys, ak)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api key rows: %w", err)
	}

	return keys, nil
}

// Revoke marks a key as revoked by its key_id. The key hash remains in the
// database for audit purposes, but Lookup will reject it with ErrKeyRevoked.
func (ks *KeyStore) Revoke(ctx context.Context, keyID string) error {
	result, err := ks.db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked', revoked_at = ? WHERE key_id = ? AND status = 'active'`,
		time.Now().UTC().Format(time.RFC3339), keyID,
	)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if rows == 0 {
		return ErrKeyNotFound
	}

	return nil
}

// hashKey computes the SHA-256 hash of a full API key string and returns
// its lowercase hex encoding. This is the value stored in key_hash column.
func hashKey(fullKey string) string {
	h := sha256.Sum256([]byte(fullKey))
	return hex.EncodeToString(h[:])
}
