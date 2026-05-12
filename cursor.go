package frostfire

type Cursor struct {
	bt    *BTree
	stack []cursorFrame
}

type cursorFrame struct {
	node *bnode
	idx  uint16
}

func (t *BTree) Cursor() *Cursor {
	return &Cursor{bt: t}
}

func (b *Bucket) Cursor() *Cursor {
	if b.dropped {
		return &Cursor{}
	}
	return b.bt.Cursor()
}

func (c *Cursor) Close() {
	for _, f := range c.stack {
		f.node.Unpin()
	}
	c.stack = nil
}

func (c *Cursor) Valid() bool {
	if len(c.stack) == 0 {
		return false
	}
	leaf := c.stack[len(c.stack)-1]
	return leaf.idx < leaf.node.nCells()
}

func (c *Cursor) Key() []byte {
	if !c.Valid() {
		return nil
	}
	leaf := c.stack[len(c.stack)-1]
	return leaf.node.key(leaf.idx)
}

func (c *Cursor) Value() ([]byte, error) {
	if !c.Valid() {
		return nil, nil
	}
	leaf := c.stack[len(c.stack)-1]
	return c.bt.leafValue(leaf.node, leaf.idx)
}

// First positions at the smallest key. Returns nil,nil if empty.
func (c *Cursor) First() ([]byte, []byte) {
	c.Close()
	if c.bt == nil || c.bt.root == 0 {
		return nil, nil
	}
	if err := c.descendLeftmost(c.bt.root); err != nil {
		return nil, nil
	}
	if !c.Valid() {
		return nil, nil
	}
	return c.kv()
}

// Last positions at the largest key. Returns nil,nil if empty.
func (c *Cursor) Last() ([]byte, []byte) {
	c.Close()
	if c.bt == nil || c.bt.root == 0 {
		return nil, nil
	}
	if err := c.descendRightmost(c.bt.root); err != nil {
		return nil, nil
	}
	if !c.Valid() {
		return nil, nil
	}
	return c.kv()
}

// Seek positions at the smallest key >= target. Returns nil,nil if no such key.
func (c *Cursor) Seek(target []byte) ([]byte, []byte) {
	c.Close()
	if c.bt == nil || c.bt.root == 0 {
		return nil, nil
	}
	id := c.bt.root
	for {
		n, err := c.bt.get(id)
		if err != nil {
			c.Close()
			return nil, nil
		}
		if n.ntype() == nodeLeaf {
			idx, _ := searchLeaf(n, target)
			c.stack = append(c.stack, cursorFrame{node: n, idx: idx})
			if idx >= n.nCells() {
				// past end of leaf; advance to next leaf
				return c.Next()
			}
			return c.kv()
		}
		idx := searchInternal(n, target)
		c.stack = append(c.stack, cursorFrame{node: n, idx: idx})
		id = n.child(idx)
	}
}

// Next advances to the next key. Returns nil,nil at end.
func (c *Cursor) Next() ([]byte, []byte) {
	if len(c.stack) == 0 {
		return nil, nil
	}
	// Try to advance within the current leaf.
	leaf := &c.stack[len(c.stack)-1]
	if leaf.idx+1 < leaf.node.nCells() {
		leaf.idx++
		return c.kv()
	}
	// Walk up until we find a frame with another child to descend into.
	c.stack[len(c.stack)-1].node.Unpin()
	c.stack = c.stack[:len(c.stack)-1]
	for len(c.stack) > 0 {
		top := &c.stack[len(c.stack)-1]
		if top.idx < top.node.nCells() {
			top.idx++
			child := top.node.child(top.idx)
			if err := c.descendLeftmost(child); err != nil {
				c.Close()
				return nil, nil
			}
			if c.Valid() {
				return c.kv()
			}
			return nil, nil
		}
		c.stack[len(c.stack)-1].node.Unpin()
		c.stack = c.stack[:len(c.stack)-1]
	}
	return nil, nil
}

// Prev steps back to the previous key. Returns nil,nil at start.
func (c *Cursor) Prev() ([]byte, []byte) {
	if len(c.stack) == 0 {
		return nil, nil
	}
	leaf := &c.stack[len(c.stack)-1]
	if leaf.idx > 0 {
		// step back within leaf; if we were past-end (idx == nCells),
		// stepping to nCells-1 puts us on the last cell
		leaf.idx--
		return c.kv()
	}
	c.stack[len(c.stack)-1].node.Unpin()
	c.stack = c.stack[:len(c.stack)-1]
	for len(c.stack) > 0 {
		top := &c.stack[len(c.stack)-1]
		if top.idx > 0 {
			top.idx--
			child := top.node.child(top.idx)
			if err := c.descendRightmost(child); err != nil {
				c.Close()
				return nil, nil
			}
			if c.Valid() {
				return c.kv()
			}
			return nil, nil
		}
		c.stack[len(c.stack)-1].node.Unpin()
		c.stack = c.stack[:len(c.stack)-1]
	}
	return nil, nil
}

func (c *Cursor) descendLeftmost(id PageId) error {
	for {
		n, err := c.bt.get(id)
		if err != nil {
			return err
		}
		if n.ntype() == nodeLeaf {
			c.stack = append(c.stack, cursorFrame{node: n, idx: 0})
			return nil
		}
		c.stack = append(c.stack, cursorFrame{node: n, idx: 0})
		id = n.child(0)
	}
}

func (c *Cursor) descendRightmost(id PageId) error {
	for {
		n, err := c.bt.get(id)
		if err != nil {
			return err
		}
		if n.ntype() == nodeLeaf {
			idx := uint16(0)
			if n.nCells() > 0 {
				idx = n.nCells() - 1
			}
			c.stack = append(c.stack, cursorFrame{node: n, idx: idx})
			return nil
		}
		nCells := n.nCells()
		c.stack = append(c.stack, cursorFrame{node: n, idx: nCells})
		id = n.child(nCells)
	}
}

func (c *Cursor) kv() ([]byte, []byte) {
	if !c.Valid() {
		return nil, nil
	}
	leaf := c.stack[len(c.stack)-1]
	k := leaf.node.key(leaf.idx)
	v, err := c.bt.leafValue(leaf.node, leaf.idx)
	if err != nil {
		return nil, nil
	}
	return k, v
}
