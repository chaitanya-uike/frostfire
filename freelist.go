package frostfire

type freelistEntry struct {
	pageID  PageId
	freedAt uint64
}

type freelist struct {
	pending []freelistEntry
}

func newFreelist() *freelist {
	return &freelist{}
}

func (f *freelist) pushMany(ids []PageId, freedAt uint64) {
	for _, id := range ids {
		f.pending = append(f.pending, freelistEntry{pageID: id, freedAt: freedAt})
	}
}

func (f *freelist) popReusable(minReaderTxn uint64) (PageId, bool) {
	if len(f.pending) == 0 || f.pending[0].freedAt > minReaderTxn {
		return 0, false
	}
	id := f.pending[0].pageID
	f.pending = f.pending[1:]
	return id, true
}

func (f *freelist) all() []PageId {
	out := make([]PageId, len(f.pending))
	for i, e := range f.pending {
		out[i] = e.pageID
	}
	return out
}

func (f *freelist) load(ids []PageId) {
	f.pending = make([]freelistEntry, len(ids))
	for i, id := range ids {
		f.pending[i] = freelistEntry{pageID: id, freedAt: 0}
	}
}

func (db *DB) walkChainIDs(head PageId) ([]PageId, error) {
	if head == 0 {
		return nil, nil
	}
	var ids []PageId
	id := head
	for id != 0 {
		page, err := db.bufferPool.Get(id)
		if err != nil {
			return nil, err
		}
		next := freelistPageNext(page.data)
		page.Unpin()
		ids = append(ids, id)
		id = next
	}
	return ids, nil
}

func (db *DB) readFreelistChain(head PageId) ([]PageId, error) {
	var all []PageId
	id := head
	for id != 0 {
		page, err := db.bufferPool.Get(id)
		if err != nil {
			return nil, err
		}
		ids, next, err := decodeFreelistPage(page.data)
		page.Unpin()
		if err != nil {
			return nil, err
		}
		all = append(all, ids...)
		id = next
	}
	return all, nil
}

func (t *Txn) persistFreelist() error {
	oldChain, err := t.db.walkChainIDs(t.meta.freelistRoot)
	if err != nil {
		return err
	}
	t.freed = append(t.freed, oldChain...)

	var chainIDs []PageId
	for {
		count := len(t.db.freelist.pending) + len(t.freed) + len(t.reusable)
		need := (count + freelistPerPage - 1) / freelistPerPage
		if len(chainIDs) >= need {
			break
		}
		page, err := t.AllocatePage()
		if err != nil {
			return err
		}
		page.Unpin()
		chainIDs = append(chainIDs, page.pageId)
	}

	if len(chainIDs) == 0 {
		t.meta.freelistRoot = 0
		return nil
	}

	persist := make([]PageId, 0, len(t.db.freelist.pending)+len(t.freed)+len(t.reusable))
	persist = append(persist, t.db.freelist.all()...)
	persist = append(persist, t.freed...)
	persist = append(persist, t.reusable...)

	for i, id := range chainIDs {
		page, err := t.db.bufferPool.GetForWrite(id)
		if err != nil {
			return err
		}
		start := i * freelistPerPage
		end := min(start+freelistPerPage, len(persist))
		var next PageId
		if i+1 < len(chainIDs) {
			next = chainIDs[i+1]
		}
		clear(page.data)
		encodeFreelistPage(page.data, persist[start:end], next)
		page.Unpin()
	}
	t.meta.freelistRoot = chainIDs[0]
	return nil
}
