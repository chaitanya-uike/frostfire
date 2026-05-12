package frostfire

import (
	"bytes"
	"encoding/binary"
)

type BTree struct {
	txn  *Txn
	root PageId
}

func NewBTree(txn *Txn, root PageId) *BTree {
	return &BTree{txn: txn, root: root}
}

func (t *BTree) Root() PageId { return t.root }

func (t *BTree) get(id PageId) (*bnode, error) {
	page, err := t.txn.Get(id)
	if err != nil {
		return nil, err
	}
	return &bnode{page: page}, nil
}

func (t *BTree) allocLeaf() (*bnode, error) {
	page, err := t.txn.AllocatePage()
	if err != nil {
		return nil, err
	}
	n := &bnode{page: page}
	n.setType(nodeLeaf)
	n.setNCells(0)
	n.setFirstCellOffset(PageSize)
	return n, nil
}

func (t *BTree) allocInternal() (*bnode, error) {
	page, err := t.txn.AllocatePage()
	if err != nil {
		return nil, err
	}
	n := &bnode{page: page}
	n.setType(nodeInternal)
	n.setNCells(0)
	n.setFirstCellOffset(PageSize)
	return n, nil
}

func (t *BTree) Search(key []byte) ([]byte, error) {
	if t.root == 0 {
		return nil, nil
	}
	n, err := t.get(t.root)
	if err != nil {
		return nil, err
	}
	for n.ntype() == nodeInternal {
		next := n.child(searchInternal(n, key))
		n.Unpin()
		n, err = t.get(next)
		if err != nil {
			return nil, err
		}
	}
	defer n.Unpin()
	idx, found := searchLeaf(n, key)
	if !found {
		return nil, nil
	}
	return t.leafValue(n, idx)
}

func (t *BTree) leafValue(n *bnode, idx uint16) ([]byte, error) {
	assert(n.ntype() == nodeLeaf, "leafValue: not a leaf")
	pos := n.cellOffset(idx)
	d := n.data()
	keylen := binary.LittleEndian.Uint16(d[pos : pos+2])
	vallen := binary.LittleEndian.Uint16(d[pos+2 : pos+4])
	if d[pos+4] == 1 {
		firstOverflow := PageId(binary.LittleEndian.Uint64(d[pos+leafCellHeader+keylen:]))
		return t.readOverflowPages(firstOverflow, int(vallen))
	}
	valStart := pos + leafCellHeader + keylen
	return d[valStart : valStart+vallen], nil
}

func searchLeaf(n *bnode, key []byte) (uint16, bool) {
	d := n.data()
	nCells := binary.LittleEndian.Uint16(d[1:3])
	const offsetBase = leafHeaderSize
	lo, hi := uint16(0), nCells
	for lo < hi {
		mid := lo + (hi-lo)/2
		offPos := offsetBase + mid*2
		cellPos := binary.LittleEndian.Uint16(d[offPos : offPos+2])
		keylen := binary.LittleEndian.Uint16(d[cellPos : cellPos+2])
		keyStart := cellPos + leafCellHeader
		cmp := bytes.Compare(key, d[keyStart:keyStart+keylen])
		switch {
		case cmp == 0:
			return mid, true
		case cmp < 0:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	return lo, false
}

func (t *BTree) FreeAll() error {
	return t.freeSubtree(t.root)
}

func (t *BTree) freeSubtree(id PageId) error {
	if id == 0 {
		return nil
	}
	n, err := t.get(id)
	if err != nil {
		return err
	}
	switch n.ntype() {
	case nodeLeaf:
		nCells := n.nCells()
		for i := range nCells {
			if ovID := n.leafCellOverflowId(i); ovID != 0 {
				if err := t.freeOverflowPages(ovID); err != nil {
					n.Unpin()
					return err
				}
			}
		}
		n.Unpin()
		t.txn.FreePage(id)
	case nodeInternal:
		nCells := n.nCells()
		children := make([]PageId, 0, nCells+1)
		for i := range nCells + 1 {
			children = append(children, n.child(i))
		}
		n.Unpin()
		for _, c := range children {
			if err := t.freeSubtree(c); err != nil {
				return err
			}
		}
		t.txn.FreePage(id)
	default:
		n.Unpin()
		panic("btree: unexpected node type in freeSubtree")
	}
	return nil
}

func searchInternal(n *bnode, key []byte) uint16 {
	d := n.data()
	nCells := binary.LittleEndian.Uint16(d[1:3])
	const offsetBase = internalHeaderSize
	lo, hi := uint16(0), nCells
	for lo < hi {
		mid := lo + (hi-lo)/2
		offPos := offsetBase + mid*2
		cellPos := binary.LittleEndian.Uint16(d[offPos : offPos+2])
		keylen := binary.LittleEndian.Uint16(d[cellPos+8 : cellPos+10])
		keyStart := cellPos + internalCellHeader
		if bytes.Compare(key, d[keyStart:keyStart+keylen]) < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}
