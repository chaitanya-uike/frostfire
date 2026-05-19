package frostfire

import (
	"bytes"
	"errors"
)

var (
	ErrBTreeNotEmpty      = errors.New("frostfire: bulk load requires an empty btree")
	ErrBulkLoaderFinished = errors.New("frostfire: bulk loader already finished")
	ErrBulkLoaderKeyOrder = errors.New("frostfire: bulk loader keys must be strictly increasing")
)

const defaultBulkLoadFillFactor = 0.7

type BulkLoadOptions struct {
	FillFactor float64
}

type BulkLoader struct {
	bt         *BTree
	spine      []*bnode
	targetUsed uint16
	lastKey    []byte
	finished   bool
}

func (t *BTree) BeginBulkLoad(opts BulkLoadOptions) (*BulkLoader, error) {
	if t.root != 0 {
		return nil, ErrBTreeNotEmpty
	}
	f := opts.FillFactor
	if f == 0 {
		f = defaultBulkLoadFillFactor
	}
	if f < 0.3 {
		f = 0.3
	}
	if f > 1.0 {
		f = 1.0
	}
	return &BulkLoader{
		bt:         t,
		targetUsed: uint16(float64(PageSize) * f),
	}, nil
}

func (l *BulkLoader) Append(key, value []byte) error {
	if l.finished {
		return ErrBulkLoaderFinished
	}
	if len(key) > maxKeySize {
		return ErrKeyTooBig
	}
	if l.lastKey != nil && bytes.Compare(key, l.lastKey) <= 0 {
		return ErrBulkLoaderKeyOrder
	}

	if len(l.spine) == 0 {
		leaf, err := l.bt.allocLeaf()
		if err != nil {
			return err
		}
		l.spine = append(l.spine, leaf)
	}

	leaf := l.spine[0]
	cellSize := leafCellSize(key, value)
	if l.shouldRollLeaf(leaf, cellSize) {
		newLeaf, err := l.bt.allocLeaf()
		if err != nil {
			return err
		}
		sepKey := append([]byte(nil), key...)
		oldLeafID := leaf.Id()
		leaf.Unpin()
		l.spine[0] = newLeaf
		if err := l.promote(1, oldLeafID, sepKey, newLeaf.Id()); err != nil {
			return err
		}
		leaf = newLeaf
	}

	overflowID, err := l.bt.leafCellOverflow(key, value)
	if err != nil {
		return err
	}
	leaf.appendLeafCellWithOverflow(key, value, overflowID)

	l.lastKey = append(l.lastKey[:0], key...)
	return nil
}

func (l *BulkLoader) Finish() (PageId, error) {
	if l.finished {
		return 0, ErrBulkLoaderFinished
	}
	l.finished = true
	if len(l.spine) == 0 {
		return 0, nil
	}
	rootID := l.spine[len(l.spine)-1].Id()
	for _, n := range l.spine {
		n.Unpin()
	}
	l.spine = nil
	l.bt.root = rootID
	return rootID, nil
}

func (l *BulkLoader) Abort() {
	if l.finished {
		return
	}
	l.finished = true
	for _, n := range l.spine {
		id := n.Id()
		n.Unpin()
		l.bt.txn.FreePage(id)
	}
	l.spine = nil
}

func (l *BulkLoader) shouldRollLeaf(leaf *bnode, cellSize uint16) bool {
	if leaf.nCells() == 0 {
		return false
	}
	if leaf.freeSpace() < cellSize+2 {
		return true
	}
	used := PageSize - leaf.freeSpace()
	return used+cellSize+2 > l.targetUsed
}

func (l *BulkLoader) shouldRollInternal(node *bnode, cellSize uint16) bool {
	if node.nCells() == 0 {
		return false
	}
	if node.freeSpace() < cellSize+2 {
		return true
	}
	used := PageSize - node.freeSpace()
	return used+cellSize+2 > l.targetUsed
}

func (l *BulkLoader) promote(level uint16, oldChild PageId, sepKey []byte, newChild PageId) error {
	if int(level) >= len(l.spine) {
		node, err := l.bt.allocInternal()
		if err != nil {
			return err
		}
		node.appendInternalCell(oldChild, sepKey)
		node.setRightmostChild(newChild)
		l.spine = append(l.spine, node)
		return nil
	}

	cur := l.spine[level]
	cellSize := internalCellSize(sepKey)
	if l.shouldRollInternal(cur, cellSize) {
		assert(cur.rightmostChild() == oldChild, "BulkLoader.promote: spine invariant broken")
		curID := cur.Id()
		cur.Unpin()
		newCur, err := l.bt.allocInternal()
		if err != nil {
			return err
		}
		newCur.setRightmostChild(newChild)
		l.spine[level] = newCur
		return l.promote(level+1, curID, sepKey, newCur.Id())
	}

	assert(cur.rightmostChild() == oldChild, "BulkLoader.promote: spine invariant broken")
	cur.appendInternalCell(oldChild, sepKey)
	cur.setRightmostChild(newChild)
	return nil
}
