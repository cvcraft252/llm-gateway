package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cvcraft252/llm-gateway/internal/db"

	_ "modernc.org/sqlite"
)

func newTestKeyStore(t *testing.T) (*db.KeyStore, *db.DB) {
	t.Helper()
	database, err := db.Init(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return db.NewKeyStore(database), database
}

func TestKeyStore_Create(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	fullKey, err := ks.Create(ctx, "test-key")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Key format: gw- + 32 hex chars = 35 total
	if !strings.HasPrefix(fullKey, "gw-") {
		t.Errorf("key should start with %q, got %q", "gw-", fullKey[:3])
	}
	if len(fullKey) != 35 {
		t.Errorf("key length = %d, want 35 (gw- + 32 hex)", len(fullKey))
	}

	hexPart := fullKey[3:]
	for _, c := range hexPart {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("key contains non-hex character %q in %q", c, hexPart)
		}
	}
}

func TestKeyStore_Create_Uniqueness(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	keys := make(map[string]bool)
	for i := 0; i < 100; i++ {
		fullKey, err := ks.Create(ctx, "uniq-test")
		if err != nil {
			t.Fatalf("Create[%d]: %v", i, err)
		}
		if keys[fullKey] {
			t.Fatalf("duplicate key generated: %s", fullKey)
		}
		keys[fullKey] = true
	}
}

func TestKeyStore_Lookup_ValidKey(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	fullKey, err := ks.Create(ctx, "production-app")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ak, err := ks.Lookup(ctx, fullKey)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ak == nil {
		t.Fatalf("Lookup: expected non-nil APIKey")
	}
	if ak.Name != "production-app" {
		t.Errorf("Name = %q, want %q", ak.Name, "production-app")
	}
	if ak.Status != "active" {
		t.Errorf("Status = %q, want %q", ak.Status, "active")
	}
	if ak.KeyID != fullKey[:12] {
		t.Errorf("KeyID = %q, want %q", ak.KeyID, fullKey[:12])
	}
}

func TestKeyStore_Lookup_InvalidKey(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	_, err := ks.Lookup(ctx, "gw-invalidkey00000000000000000000")
	if !errors.Is(err, db.ErrKeyNotFound) {
		t.Errorf("Lookup with invalid key: error = %v, want ErrKeyNotFound", err)
	}
}

func TestKeyStore_Lookup_TooShort(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	_, err := ks.Lookup(ctx, "short")
	if !errors.Is(err, db.ErrKeyNotFound) {
		t.Errorf("Lookup with short key: error = %v, want ErrKeyNotFound", err)
	}
}

func TestKeyStore_Lookup_WrongHash(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	fullKey, _ := ks.Create(ctx, "test")

	// Tamper with the key: same key_id prefix but different suffix
	tampered := fullKey[:12] + "fffffffffffffffffffffffffffff"
	_, err := ks.Lookup(ctx, tampered)
	if !errors.Is(err, db.ErrKeyNotFound) {
		t.Errorf("Lookup with tampered key: error = %v, want ErrKeyNotFound", err)
	}
}

func TestKeyStore_Revoke(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	fullKey, _ := ks.Create(ctx, "to-revoke")

	// Verify key works before revocation
	_, err := ks.Lookup(ctx, fullKey)
	if err != nil {
		t.Fatalf("Lookup before revoke: %v", err)
	}

	// Revoke
	keyID := fullKey[:12]
	if err := ks.Revoke(ctx, keyID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Verify key is rejected after revocation
	_, err = ks.Lookup(ctx, fullKey)
	if !errors.Is(err, db.ErrKeyRevoked) {
		t.Errorf("Lookup after revoke: error = %v, want ErrKeyRevoked", err)
	}
}

func TestKeyStore_Revoke_AlreadyRevoked(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	fullKey, _ := ks.Create(ctx, "double-revoke")
	keyID := fullKey[:12]

	if err := ks.Revoke(ctx, keyID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}

	err := ks.Revoke(ctx, keyID)
	if !errors.Is(err, db.ErrKeyNotFound) {
		t.Errorf("second Revoke: error = %v, want ErrKeyNotFound (no active row to update)", err)
	}
}

func TestKeyStore_Revoke_Nonexistent(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	err := ks.Revoke(ctx, "gw-nonexist")
	if !errors.Is(err, db.ErrKeyNotFound) {
		t.Errorf("Revoke nonexistent: error = %v, want ErrKeyNotFound", err)
	}
}

func TestKeyStore_List(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)
	ctx := context.Background()

	// Create 3 keys, revoke 1
	key1, _ := ks.Create(ctx, "key-one")
	_, _ = ks.Create(ctx, "key-two")
	key3, _ := ks.Create(ctx, "key-three")
	_ = ks.Revoke(ctx, key1[:12])

	// List all
	all, err := ks.List(ctx, "")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List all: count = %d, want 3", len(all))
	}

	// List active only
	active, err := ks.List(ctx, "active")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("List active: count = %d, want 2", len(active))
	}
	for _, ak := range active {
		if ak.Status != "active" {
			t.Errorf("List active returned non-active key: %s status=%s", ak.KeyID, ak.Status)
		}
	}

	// List revoked only
	revoked, err := ks.List(ctx, "revoked")
	if err != nil {
		t.Fatalf("List revoked: %v", err)
	}
	if len(revoked) != 1 {
		t.Errorf("List revoked: count = %d, want 1", len(revoked))
	}
	if revoked[0].KeyID != key1[:12] {
		t.Errorf("revoked key KeyID = %q, want %q", revoked[0].KeyID, key1[:12])
	}
	if !revoked[0].RevokedAt.Valid {
		t.Errorf("revoked key should have non-NULL revoked_at")
	}

	// Verify key3 is in active list
	found := false
	for _, ak := range active {
		if ak.KeyID == key3[:12] {
			found = true
		}
	}
	if !found {
		t.Errorf("key3 (%s) not found in active list", key3[:12])
	}
}

func TestKeyStore_Lookup_ContextTimeout(t *testing.T) {
	t.Parallel()

	ks, _ := newTestKeyStore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(1 * time.Millisecond)

	fullKey, _ := ks.Create(context.Background(), "timeout-test")
	_, err := ks.Lookup(ctx, fullKey)
	if err == nil {
		t.Errorf("Lookup with expired context: expected error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// Some drivers wrap the error; just check it's not nil and not success
		t.Logf("Lookup with expired context: error = %v (not exactly DeadlineExceeded but non-nil)", err)
	}
}
