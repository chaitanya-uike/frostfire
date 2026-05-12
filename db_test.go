package frostfire

import (
	"path/filepath"
	"testing"
)

func TestOpenNewDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	m := db.currentMeta.Load()
	if m == nil {
		t.Fatal("currentMeta is nil after Open")
	}
	if m.magic != metaMagic {
		t.Errorf("magic: got %x want %x", m.magic, metaMagic)
	}
	if m.version != metaVersion {
		t.Errorf("version: got %d want %d", m.version, metaVersion)
	}
	if m.pageSize != PageSize {
		t.Errorf("pageSize: got %d want %d", m.pageSize, PageSize)
	}
	if m.txnID != 0 {
		t.Errorf("txnID: got %d want 0", m.txnID)
	}
	if db.currentMetaPage != metaPage0 {
		t.Errorf("currentMetaPage: got %d want %d", db.currentMetaPage, metaPage0)
	}
}

func TestReopenDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open new: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open existing: %v", err)
	}
	defer db2.Close()

	m := db2.currentMeta.Load()
	if m.txnID != 0 {
		t.Errorf("reopened txnID: got %d want 0", m.txnID)
	}
}

func TestCommitMetaAlternatesPages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if db.currentMetaPage != metaPage0 {
		t.Fatalf("initial page: got %d want %d", db.currentMetaPage, metaPage0)
	}

	// First commit: should land on page 1.
	newMeta := &Meta{
		magic:        metaMagic,
		version:      metaVersion,
		pageSize:     PageSize,
		txnID:        1,
		catalogRoot:  42,
		freelistRoot: 0,
		numPages:     2,
	}
	db.writerMu.Lock()
	err = db.commitMeta(newMeta)
	db.writerMu.Unlock()
	if err != nil {
		t.Fatalf("commitMeta: %v", err)
	}
	if db.currentMetaPage != metaPage1 {
		t.Errorf("after first commit: got %d want %d", db.currentMetaPage, metaPage1)
	}

	// Second commit: back to page 0.
	newMeta2 := &Meta{
		magic:        metaMagic,
		version:      metaVersion,
		pageSize:     PageSize,
		txnID:        2,
		catalogRoot:  43,
		freelistRoot: 0,
		numPages:     3,
	}
	db.writerMu.Lock()
	err = db.commitMeta(newMeta2)
	db.writerMu.Unlock()
	if err != nil {
		t.Fatalf("commitMeta 2: %v", err)
	}
	if db.currentMetaPage != metaPage0 {
		t.Errorf("after second commit: got %d want %d", db.currentMetaPage, metaPage0)
	}
}

func TestCommitMetaSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	newMeta := &Meta{
		magic:        metaMagic,
		version:      metaVersion,
		pageSize:     PageSize,
		txnID:        7,
		catalogRoot:  99,
		freelistRoot: 0,
		numPages:     2,
	}
	db.writerMu.Lock()
	err = db.commitMeta(newMeta)
	db.writerMu.Unlock()
	if err != nil {
		t.Fatalf("commitMeta: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	m := db2.currentMeta.Load()
	if m.txnID != 7 {
		t.Errorf("reopened txnID: got %d want 7", m.txnID)
	}
	if m.catalogRoot != 99 {
		t.Errorf("reopened catalogRoot: got %d want 99", m.catalogRoot)
	}
	if db2.currentMetaPage != metaPage1 {
		t.Errorf("reopened currentMetaPage: got %d want %d", db2.currentMetaPage, metaPage1)
	}
}

func TestCorruptOneMetaStillRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frost.db")

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Commit twice so both meta pages have valid content with different txnIDs.
	for txnID := uint64(1); txnID <= 2; txnID++ {
		m := &Meta{
			magic:        metaMagic,
			version:      metaVersion,
			pageSize:     PageSize,
			txnID:        txnID,
			catalogRoot:  PageId(100 + txnID),
			freelistRoot: 0,
			numPages:     2,
		}
		db.writerMu.Lock()
		if err := db.commitMeta(m); err != nil {
			db.writerMu.Unlock()
			t.Fatalf("commitMeta %d: %v", txnID, err)
		}
		db.writerMu.Unlock()
	}
	// After two commits the most recent (txnID=2) is on metaPage0.
	if db.currentMetaPage != metaPage0 {
		t.Fatalf("expected metaPage0 to be current, got %d", db.currentMetaPage)
	}

	// Corrupt page 0 in the buffer pool (the newer one).
	page, err := db.bufferPool.GetForWrite(metaPage0)
	if err != nil {
		t.Fatalf("GetForWrite: %v", err)
	}
	for i := range metaSize {
		page.data[i] = 0xFF
	}
	page.Unpin()
	if err := db.bufferPool.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if err := db.bufferPool.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: page 0 is corrupt, page 1 (txnID=1) should win.
	db2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	m := db2.currentMeta.Load()
	if m.txnID != 1 {
		t.Errorf("recovered txnID: got %d want 1 (fell back to older valid meta)", m.txnID)
	}
}
