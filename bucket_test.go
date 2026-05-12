package frostfire

import (
	"bytes"
	"path/filepath"
	"testing"
)

func openTestDBOnly(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db, path
}

func TestBucketCreateAndPutGet(t *testing.T) {
	db, _ := openTestDBOnly(t)

	txn := db.BeginWrite()
	b, err := txn.CreateBucket([]byte("users"))
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := b.Put([]byte("alice"), []byte("data-a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Put([]byte("bob"), []byte("data-b")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get([]byte("alice"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("data-a")) {
		t.Errorf("Get(alice): got %q want %q", got, "data-a")
	}
	txn.Abort()
}

func TestBucketPersistsAcrossCommit(t *testing.T) {
	db, _ := openTestDBOnly(t)

	wt := db.BeginWrite()
	b, err := wt.CreateBucket([]byte("users"))
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := b.Put([]byte("alice"), []byte("data-a")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := wt.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rt := db.BeginRead()
	defer rt.Abort()
	b2, err := rt.Bucket([]byte("users"))
	if err != nil {
		t.Fatalf("Bucket after commit: %v", err)
	}
	got, err := b2.Get([]byte("alice"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("data-a")) {
		t.Errorf("Get after commit: got %q want %q", got, "data-a")
	}
}

func TestBucketPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	wt := db.BeginWrite()
	b, err := wt.CreateBucket([]byte("logs"))
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for i := range 50 {
		k := []byte{byte(i)}
		if err := b.Put(k, []byte{byte(i ^ 0xAA)}); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if err := wt.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	rt := db2.BeginRead()
	defer rt.Abort()
	b2, err := rt.Bucket([]byte("logs"))
	if err != nil {
		t.Fatalf("Bucket after reopen: %v", err)
	}
	for i := range 50 {
		got, err := b2.Get([]byte{byte(i)})
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		want := byte(i ^ 0xAA)
		if len(got) != 1 || got[0] != want {
			t.Errorf("key %d: got %v want [%d]", i, got, want)
		}
	}
}

func TestBucketCreateExistsErrors(t *testing.T) {
	db, _ := openTestDBOnly(t)

	wt := db.BeginWrite()
	if _, err := wt.CreateBucket([]byte("dup")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := wt.CreateBucket([]byte("dup")); err != ErrBucketExists {
		t.Errorf("expected ErrBucketExists, got %v", err)
	}
	if err := wt.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	wt2 := db.BeginWrite()
	defer wt2.Abort()
	if _, err := wt2.CreateBucket([]byte("dup")); err != ErrBucketExists {
		t.Errorf("after commit: expected ErrBucketExists, got %v", err)
	}
}

func TestBucketMissingReturnsErr(t *testing.T) {
	db, _ := openTestDBOnly(t)
	rt := db.BeginRead()
	defer rt.Abort()
	if _, err := rt.Bucket([]byte("nope")); err != ErrBucketNotFound {
		t.Errorf("expected ErrBucketNotFound, got %v", err)
	}
}

func TestBucketDropRemovesEntries(t *testing.T) {
	db, _ := openTestDBOnly(t)

	wt := db.BeginWrite()
	b, err := wt.CreateBucket([]byte("scratch"))
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for i := range 100 {
		if err := b.Put([]byte{byte(i)}, []byte{byte(i)}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := wt.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	wt2 := db.BeginWrite()
	if err := wt2.DropBucket([]byte("scratch")); err != nil {
		t.Fatalf("DropBucket: %v", err)
	}
	if err := wt2.Commit(); err != nil {
		t.Fatalf("Commit drop: %v", err)
	}

	rt := db.BeginRead()
	defer rt.Abort()
	if _, err := rt.Bucket([]byte("scratch")); err != ErrBucketNotFound {
		t.Errorf("after drop: expected ErrBucketNotFound, got %v", err)
	}
}

func TestBucketDropMissingErrors(t *testing.T) {
	db, _ := openTestDBOnly(t)
	wt := db.BeginWrite()
	defer wt.Abort()
	if err := wt.DropBucket([]byte("nope")); err != ErrBucketNotFound {
		t.Errorf("expected ErrBucketNotFound, got %v", err)
	}
}

func TestBucketsAreIsolated(t *testing.T) {
	db, _ := openTestDBOnly(t)

	wt := db.BeginWrite()
	a, err := wt.CreateBucket([]byte("a"))
	if err != nil {
		t.Fatalf("CreateBucket(a): %v", err)
	}
	bb, err := wt.CreateBucket([]byte("b"))
	if err != nil {
		t.Fatalf("CreateBucket(b): %v", err)
	}
	if err := a.Put([]byte("k"), []byte("from-a")); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := bb.Put([]byte("k"), []byte("from-b")); err != nil {
		t.Fatalf("Put b: %v", err)
	}

	gotA, _ := a.Get([]byte("k"))
	if !bytes.Equal(gotA, []byte("from-a")) {
		t.Errorf("a[k] = %q, want %q", gotA, "from-a")
	}
	gotB, _ := bb.Get([]byte("k"))
	if !bytes.Equal(gotB, []byte("from-b")) {
		t.Errorf("b[k] = %q, want %q", gotB, "from-b")
	}
	wt.Abort()
}

func TestBucketCreateDropCreateInOneTxn(t *testing.T) {
	db, _ := openTestDBOnly(t)

	wt := db.BeginWrite()
	b, err := wt.CreateBucket([]byte("temp"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Put([]byte("key"), []byte("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := wt.DropBucket([]byte("temp")); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	b2, err := wt.CreateBucket([]byte("temp"))
	if err != nil {
		t.Fatalf("Create-after-drop: %v", err)
	}
	if err := b2.Put([]byte("key"), []byte("v2")); err != nil {
		t.Fatalf("Put on recreated: %v", err)
	}
	got, err := b2.Get([]byte("key"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("v2")) {
		t.Errorf("got %q want %q", got, "v2")
	}
	if err := wt.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rt := db.BeginRead()
	defer rt.Abort()
	b3, err := rt.Bucket([]byte("temp"))
	if err != nil {
		t.Fatalf("Bucket after commit: %v", err)
	}
	got2, _ := b3.Get([]byte("key"))
	if !bytes.Equal(got2, []byte("v2")) {
		t.Errorf("after commit: got %q want %q", got2, "v2")
	}
}

func TestBucketReadTxnRejectsWrites(t *testing.T) {
	db, _ := openTestDBOnly(t)

	rt := db.BeginRead()
	defer rt.Abort()
	if _, err := rt.CreateBucket([]byte("x")); err != ErrTxnReadOnly {
		t.Errorf("CreateBucket on read txn: got %v want ErrTxnReadOnly", err)
	}
	if err := rt.DropBucket([]byte("x")); err != ErrTxnReadOnly {
		t.Errorf("DropBucket on read txn: got %v want ErrTxnReadOnly", err)
	}
}

func TestBucketInsertRejectsDuplicate(t *testing.T) {
	db, _ := openTestDBOnly(t)
	wt := db.BeginWrite()
	defer wt.Abort()
	b, err := wt.CreateBucket([]byte("t"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Insert([]byte("k"), []byte("v1")); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err = b.Insert([]byte("k"), []byte("v2"))
	if err != ErrKeyExists {
		t.Errorf("expected ErrKeyExists, got %v", err)
	}
	got, _ := b.Get([]byte("k"))
	if !bytes.Equal(got, []byte("v1")) {
		t.Errorf("value clobbered: got %q want %q", got, "v1")
	}
}

func TestBucketUpdateRequiresExisting(t *testing.T) {
	db, _ := openTestDBOnly(t)
	wt := db.BeginWrite()
	defer wt.Abort()
	b, err := wt.CreateBucket([]byte("t"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	err = b.Update([]byte("missing"), []byte("v"))
	if err != ErrKeyNotFound {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}
	if err := b.Insert([]byte("k"), []byte("v1")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := b.Update([]byte("k"), []byte("v2")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := b.Get([]byte("k"))
	if !bytes.Equal(got, []byte("v2")) {
		t.Errorf("after Update: got %q want %q", got, "v2")
	}
}

func TestBucketHandleCachedAcrossLookups(t *testing.T) {
	db, _ := openTestDBOnly(t)

	wt := db.BeginWrite()
	defer wt.Abort()
	b1, err := wt.CreateBucket([]byte("c"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b2, err := wt.Bucket([]byte("c"))
	if err != nil {
		t.Fatalf("Bucket: %v", err)
	}
	if b1 != b2 {
		t.Errorf("expected same *Bucket from cache, got distinct pointers")
	}
}
