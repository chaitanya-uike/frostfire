package frostfire

import (
	"errors"
	"sort"
	"sync"
)

type PageState uint8

const (
	Empty PageState = iota
	Loading
	Ready
	Evicting
)

const (
	maxRefCount = 5
	tableShards = 64
	freePageId  = PageId(^uint64(0))
)

var (
	ErrBufferExhausted = errors.New("BufferPool: no unpinned buffers available")
)

type Page struct {
	pageId PageId

	state PageState

	data []byte

	pinCount int32
	refCount int32
	dirty    bool

	mu sync.Mutex

	// condition var to signal loading and eviction state waiters
	c sync.Cond
}

type Shard struct {
	mu sync.Mutex
	m  map[PageId]*Page
}

type BufferPool struct {
	frames    []*Page
	pageTable [tableShards]*Shard

	clockHand int
	mu        sync.Mutex

	sm *StorageManager
}

func NewBufferPool(sm *StorageManager, size int) *BufferPool {
	frames := make([]*Page, size)
	for i := range size {
		page := &Page{
			pageId: freePageId,
			state:  Empty,
			data:   NewBuffer(),
		}
		page.c = *sync.NewCond(&page.mu)
		frames[i] = page
	}
	p := &BufferPool{
		frames:    frames,
		pageTable: [tableShards]*Shard{},
		sm:        sm,
	}
	for i := range tableShards {
		p.pageTable[i] = &Shard{m: map[PageId]*Page{}}
	}
	return p
}

func (p *BufferPool) getShard(pageId PageId) *Shard {
	return p.pageTable[pageId%tableShards]
}

func (p *BufferPool) Get(pageId PageId) (*Page, error) {
	for {
		shard := p.getShard(pageId)

		shard.mu.Lock()
		if exPage, ok := shard.m[pageId]; ok {
			exPage.mu.Lock()
			shard.mu.Unlock()

			switch exPage.state {
			case Ready:
				exPage.pinCount++
				exPage.refCount = maxRefCount
				exPage.mu.Unlock()
				return exPage, nil

			case Loading:
				for exPage.state == Loading {
					exPage.c.Wait()
				}

				if exPage.state != Ready {
					exPage.mu.Unlock()
					continue
				}
				exPage.pinCount++
				exPage.refCount = maxRefCount
				exPage.mu.Unlock()
				return exPage, nil

			case Evicting:
				for exPage.state == Evicting {
					exPage.c.Wait()
				}
				exPage.mu.Unlock()
				continue

			default:
				exPage.mu.Unlock()
				panic("bufferpool: invalid state for a frame in pageTable")
			}
		}
		shard.mu.Unlock()

		frame, err := p.acquireFrame()
		if err != nil {
			return nil, err
		}

		shard.mu.Lock()
		if exPage, ok := shard.m[pageId]; ok {
			frame.pinCount = 0
			frame.mu.Unlock()
			exPage.mu.Lock()
			shard.mu.Unlock()

			if exPage.state == Ready {
				exPage.pinCount++
				exPage.refCount = maxRefCount
				exPage.mu.Unlock()
				return exPage, nil
			}
			exPage.mu.Unlock()
			continue
		}

		frame.pageId = pageId
		frame.state = Loading
		shard.m[pageId] = frame
		shard.mu.Unlock()

		data := frame.data
		frame.mu.Unlock()

		readErr := p.sm.ReadPage(pageId, data)

		frame.mu.Lock()
		if readErr != nil {
			shard.mu.Lock()
			delete(shard.m, pageId)
			shard.mu.Unlock()

			frame.pageId = freePageId
			frame.state = Empty
			frame.pinCount = 0
			frame.refCount = 0
			frame.c.Broadcast()
			frame.mu.Unlock()
			return nil, readErr
		}

		frame.state = Ready
		frame.c.Broadcast()
		frame.mu.Unlock()
		return frame, nil
	}
}

func (p *BufferPool) AllocatePage(pageId PageId) (*Page, error) {
	if err := p.sm.Allocate(pageId); err != nil {
		return nil, err
	}
	return p.GetForWrite(pageId)
}

func (p *BufferPool) GetForWrite(pageId PageId) (*Page, error) {
	shard := p.getShard(pageId)

	shard.mu.Lock()
	if exPage, ok := shard.m[pageId]; ok {
		exPage.mu.Lock()
		shard.mu.Unlock()

		if exPage.state != Ready {
			exPage.mu.Unlock()
			panic("bufferpool: GetForWrite on non-Ready resident page")
		}

		clear(exPage.data)
		exPage.pinCount = 1
		exPage.refCount = maxRefCount
		exPage.dirty = true
		exPage.mu.Unlock()
		return exPage, nil
	}
	shard.mu.Unlock()

	frame, err := p.acquireFrame()
	if err != nil {
		return nil, err
	}

	shard.mu.Lock()
	clear(frame.data)
	frame.pageId = pageId
	frame.state = Ready
	frame.dirty = true
	shard.m[pageId] = frame
	shard.mu.Unlock()
	frame.mu.Unlock()
	return frame, nil
}

func (p *BufferPool) acquireFrame() (*Page, error) {
	p.mu.Lock()
	start := p.clockHand
	sawUnpinned := false

	for {
		p.clockHand = (p.clockHand + 1) % len(p.frames)
		if p.clockHand == start {
			if !sawUnpinned {
				p.mu.Unlock()
				return nil, ErrBufferExhausted
			}
			sawUnpinned = false
		}

		frame := p.frames[p.clockHand]
		frame.mu.Lock()

		if frame.pinCount > 0 {
			frame.mu.Unlock()
			continue
		}
		sawUnpinned = true

		if frame.state == Empty {
			frame.pinCount = 1
			frame.refCount = maxRefCount
			p.mu.Unlock()
			return frame, nil
		}

		if frame.state != Ready {
			frame.mu.Unlock()
			continue
		}

		if frame.refCount > 0 {
			frame.refCount--
			frame.mu.Unlock()
			continue
		}

		oldPageId := frame.pageId
		wasDirty := frame.dirty
		data := frame.data
		frame.state = Evicting

		p.mu.Unlock()
		frame.mu.Unlock()

		if wasDirty {
			if err := p.sm.WritePage(oldPageId, data); err != nil {
				frame.mu.Lock()
				frame.state = Ready
				// Wake any Get(oldPageId) waiters parked on state == Evicting.
				frame.c.Broadcast()
				frame.mu.Unlock()
				p.mu.Lock()
				continue
			}
		}

		shard := p.getShard(oldPageId)
		shard.mu.Lock()
		delete(shard.m, oldPageId)
		shard.mu.Unlock()

		frame.mu.Lock()

		frame.pageId = freePageId
		frame.state = Empty
		frame.dirty = false
		frame.pinCount = 1
		frame.refCount = maxRefCount

		// Wake any Get(oldPageId) waiters parked on state == Evicting.
		frame.c.Broadcast()

		return frame, nil
	}
}

func (p *BufferPool) FlushPage(pageId PageId) error {
	shard := p.getShard(pageId)
	shard.mu.Lock()
	frame, ok := shard.m[pageId]
	if !ok {
		shard.mu.Unlock()
		return nil
	}
	frame.mu.Lock()
	shard.mu.Unlock()

	if frame.state != Ready || !frame.dirty {
		frame.mu.Unlock()
		return nil
	}

	data := frame.data
	frame.mu.Unlock()

	if err := p.sm.WritePage(pageId, data); err != nil {
		return err
	}

	frame.mu.Lock()
	frame.dirty = false
	frame.mu.Unlock()
	return nil
}

type dirtyFrame struct {
	pageId PageId
	data   []byte
	frame  *Page
}

func (p *BufferPool) FlushPages(ids []PageId) error {
	dirty := make([]dirtyFrame, 0, len(ids))
	for _, pageId := range ids {
		shard := p.getShard(pageId)
		shard.mu.Lock()
		frame, ok := shard.m[pageId]
		if !ok {
			shard.mu.Unlock()
			continue
		}
		frame.mu.Lock()
		shard.mu.Unlock()

		if frame.state != Ready || !frame.dirty {
			frame.mu.Unlock()
			continue
		}
		dirty = append(dirty, dirtyFrame{
			pageId: pageId,
			data:   frame.data,
			frame:  frame,
		})
		frame.mu.Unlock()
	}
	return p.flushDirty(dirty)
}

func (p *BufferPool) FlushAll() error {
	dirty := make([]dirtyFrame, 0, len(p.frames))
	for _, frame := range p.frames {
		frame.mu.Lock()
		if frame.state != Ready || !frame.dirty {
			frame.mu.Unlock()
			continue
		}
		dirty = append(dirty, dirtyFrame{
			pageId: frame.pageId,
			data:   frame.data,
			frame:  frame,
		})
		frame.mu.Unlock()
	}
	return p.flushDirty(dirty)
}

func (p *BufferPool) flushDirty(dirty []dirtyFrame) error {
	sort.Slice(dirty, func(i, j int) bool {
		return dirty[i].pageId < dirty[j].pageId
	})

	for i := 0; i < len(dirty); {
		j := i + 1
		for j < len(dirty) && dirty[j].pageId == dirty[j-1].pageId+1 {
			j++
		}
		bufs := make([][]byte, 0, j-i)
		for k := i; k < j; k++ {
			bufs = append(bufs, dirty[k].data)
		}
		if err := p.sm.WritePagesContiguous(dirty[i].pageId, bufs); err != nil {
			return err
		}
		for k := i; k < j; k++ {
			dirty[k].frame.mu.Lock()
			dirty[k].frame.dirty = false
			dirty[k].frame.mu.Unlock()
		}
		i = j
	}
	return nil
}

func (p *BufferPool) MarkClean(pageID PageId) {
	shard := p.getShard(pageID)
	shard.mu.Lock()
	frame, ok := shard.m[pageID]
	if !ok {
		shard.mu.Unlock()
		return
	}
	frame.mu.Lock()
	shard.mu.Unlock()
	frame.dirty = false
	frame.mu.Unlock()
}

func (pg *Page) Unpin() {
	pg.mu.Lock()
	if pg.pinCount <= 0 {
		pg.mu.Unlock()
		panic("bufferpool: Unpin on page with pinCount <= 0")
	}
	pg.pinCount--
	pg.mu.Unlock()
}

func (p *BufferPool) Sync() error {
	return p.sm.Sync()
}

func (p *BufferPool) Close() error {
	flushErr := p.FlushAll()
	closeErr := p.sm.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}
