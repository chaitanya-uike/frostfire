# Frostfire

Frostfire is an embedded, ACID-compliant key/value store written in Go. It runs
in-process against a single file and exposes a transactional API over a
copy-on-write B+tree.

Requirements: Go 1.25 or later, on Linux or macOS.

## Installation

```sh
go get github.com/chaitanya-uike/frostfire
```

## Opening A Database

```go
db, err := frostfire.Open("data.db", frostfire.Options{})
if err != nil {
    return err
}
defer db.Close()
```

If the file does not exist, Frostfire creates and initializes it. If it exists,
Frostfire resumes from the last committed state.

`Options`:

| Field        | Default | Meaning                                                       |
| ------------ | ------- | ------------------------------------------------------------- |
| `BufferSize` | `1024`  | Number of 4 KiB page frames held in memory (~4 MiB at default). |

`db.Close()` flushes pending writes and releases the file.

## Transactions

All work happens inside a transaction. There are two kinds:

- **Read transactions** are obtained from `db.BeginRead()`. They observe a
  stable snapshot and may run concurrently with other readers and with the
  writer. They cannot modify data.
- **Write transactions** are obtained from `db.BeginWrite()`. Only one write
  transaction may be active at a time. Other writers block until it commits or
  aborts.

Every transaction must be terminated with either `Commit()` or `Abort()`.
Forgetting to do so leaks resources. Forgetting to end a write transaction also
blocks every future writer.

```go
txn := db.BeginWrite()
defer txn.Abort() // safe no-op if Commit succeeded

// ... do work ...

if err := txn.Commit(); err != nil {
    return err
}
```

When `Commit` returns nil, the transaction's changes have been written and
synced. `Abort` discards all changes made by the transaction.

### Transaction Guidelines

Write transactions serialize. Treat them as short critical sections:

- do not hold a write transaction while doing network I/O, sleeping, waiting for
  user input, or performing unrelated work;
- batch related writes into one transaction when possible;
- avoid extremely large batches if they make other writers wait too long;
- always `Abort` on early returns before `Commit`;
- keep transaction ownership in one goroutine.

The last point is important. A `Txn`, `Bucket`, or `Cursor` should not be passed
between goroutines. Let one goroutine own the transaction from begin to
commit/abort.

### Why Batch Writes

Every committed write transaction has fixed costs. Frostfire must flush dirty
pages, persist the freelist when it changes, write the next meta page, and sync
the file for durability.

This pattern pays those costs once per item:

```go
for _, item := range items {
    tx := db.BeginWrite()
    b, err := tx.Bucket([]byte("items"))
    if err != nil {
        tx.Abort()
        return err
    }
    if err := b.Put(item.Key, item.Value); err != nil {
        tx.Abort()
        return err
    }
    if err := tx.Commit(); err != nil {
        return err
    }
}
```

This pattern pays them once for the whole batch:

```go
tx := db.BeginWrite()
defer tx.Abort()

b, err := tx.Bucket([]byte("items"))
if err != nil {
    return err
}
for _, item := range items {
    if err := b.Put(item.Key, item.Value); err != nil {
        return err
    }
}
return tx.Commit()
```

The tradeoff is latency. Larger batches amortize commit cost better, but they
also hold the single writer slot for longer.

## Buckets

A bucket is a named, ordered key/value namespace. Buckets are created, looked
up, and dropped through a transaction.

```go
txn := db.BeginWrite()
defer txn.Abort()

users, err := txn.CreateBucket([]byte("users"))
if err != nil {
    return err
}

if err := txn.Commit(); err != nil {
    return err
}
```

`CreateBucket` returns `ErrBucketExists` if the name is already in use.
`txn.Bucket(name)` looks up an existing bucket and returns `ErrBucketNotFound`
if it does not exist. It works in both read and write transactions.
`txn.DropBucket(name)` removes a bucket and frees all pages it owns.

### Reading And Writing Keys

Within a bucket, four operations modify entries:

| Method               | Behavior                                                           |
| -------------------- | ------------------------------------------------------------------ |
| `Put(key, value)`    | Insert or replace. Always succeeds if the value fits.              |
| `Insert(key, value)` | Insert only. Returns `ErrKeyExists` if the key is already present. |
| `Update(key, value)` | Replace only. Returns `ErrKeyNotFound` if the key is absent.       |
| `Delete(key)`        | Remove the key. No error if the key was already absent.            |

`Get(key)` returns the value, or `(nil, nil)` if the key does not exist.

Keys are compared lexicographically as raw bytes. If you encode numbers or
compound keys, use an encoding that preserves the order you want when compared
byte by byte.

### Bulk Loading

When initially populating an empty bucket from sorted input, `BeginBulkLoad`
constructs the B+tree bottom-up without copy-on-write. Bulk Loading is faster than calling `Put` in a loop.

```go
bl, err := b.BeginBulkLoad(frostfire.BulkLoadOptions{FillFactor: 0.7})
if err != nil {
    return err
}
for _, item := range sortedItems {
    if err := bl.Append(item.Key, item.Value); err != nil {
        bl.Abort()
        return err
    }
}
if _, err := bl.Finish(); err != nil {
    return err
}
```

Contract:

- The bucket's B+tree must be empty when `BeginBulkLoad` is called. Otherwise
  it returns `ErrBTreeNotEmpty`.
- Keys passed to `Append` must be strictly increasing. Out-of-order keys
  return `ErrBulkLoaderKeyOrder`.
- The caller must not allow concurrent readers of this bucket until `Finish`
  has been called. Inside a single write transaction this is automatic; if
  you fan out work across goroutines, hold all bucket access in one of them.
- Call `Finish` to publish the new tree, or `Abort` to discard it. After
  either, the `BulkLoader` is dead.

`BulkLoadOptions.FillFactor` controls how full each new page is packed before
rolling to a sibling. The default (`0`) is `0.7`, which leaves headroom for
subsequent random writes without immediately splitting pages. `1.0` packs to
maximum density (best for read-only snapshots, worst for further writes).
Values are clamped to `[0.3, 1.0]`.

### Iteration

Each bucket exposes a cursor that walks its keys in sorted order:

```go
c := users.Cursor()
defer c.Close()

for k, v := c.First(); k != nil; k, v = c.Next() {
    fmt.Printf("%s = %s\n", k, v)
}
```

Cursor methods all return `(key, value)`, or `(nil, nil)` once iteration is
exhausted:

| Method         | Behavior                                      |
| -------------- | --------------------------------------------- |
| `First()`      | Move to the first key in the bucket.          |
| `Last()`       | Move to the last key in the bucket.           |
| `Seek(target)` | Move to the first key greater than or equal to `target`. |
| `Next()`       | Advance one position.                         |
| `Prev()`       | Step back one position.                       |

Cursors hold pinned pages while open. Call `Close()` when finished or the buffer
pool can steadily fill with pages that cannot be evicted.

## How It Works

Frostfire is inspired by [LMDB](http://www.lmdb.tech/doc/) and
[BoltDB](https://github.com/boltdb/bolt): a single-file embedded store, one
writer at a time, snapshot reads, and data laid out as a copy-on-write B+tree of
fixed-size pages.

### Mental Model

Immutable Datastructures: The important idea is that updates do not overwrite
pages that a reader might still be using. A write transaction copies the pages it
changes, updates parent pages to point at those copies, and eventually publishes
a new root page by writing a new meta page. Readers keep walking the root they
saw when their transaction began.

That gives Frostfire a simple shape:

- readers get stable snapshots,
- one writer builds the next version of the tree,
- old pages become reusable only when no active reader can still see them,
- commit publishes the new version durably.

### Copy-On-Write B+tree

Each bucket is a B+tree. Leaf pages contain keys and values. Internal pages
contain separator keys and child page IDs.

Updating a key copies the leaf that contains it. If the copied leaf splits, a
separator key is pushed into the parent. If the parent must change, it is copied
too. This continues up to the root, producing a new root page for the
transaction.

Delete follows the same copy-on-write rule. It copies the affected child, then
repairs underfull pages by borrowing from siblings or merging pages. Parent
pages are copied as their child pointers and separator keys change.

The old pages are not overwritten. They remain valid for any read transaction
that started before the writer committed.

Bulk loading is the one exception. `BeginBulkLoad` requires an empty B+tree,
so no reader can be holding any of its pages, and the loader writes cells
directly into freshly allocated pages instead of allocating-then-copying on
every insert. The resulting tree shape is identical to what a sequence of
`Put` calls would produce.

### Snapshots And Page Reuse

A read transaction captures the current meta page when it begins. That meta page
names the catalog root, freelist root, page count, and transaction ID. Reads walk
only the pages reachable from that snapshot.

When a write transaction frees a page, Frostfire cannot always reuse it
immediately. An older reader may still be able to reach that page from its
snapshot. Freed pages enter the freelist with the transaction that freed them,
and they become reusable only after active readers are new enough.

Long-lived read transactions are therefore cheap for concurrency, but not free:
they can delay page reuse.

### Buffer Pool And Direct I/O

LMDB and BoltDB rely on `mmap` and let the kernel page cache do the buffering.
Frostfire manages its own buffer pool and reads/writes pages through direct I/O
(`O_DIRECT` on Linux, `F_NOCACHE` on macOS), bypassing the page cache.

The buffer pool keeps a fixed number of page frames in memory. Pages are pinned
while code is using them, dirty when modified, and evicted with a clock-style
replacement policy when space is needed. Pinned pages cannot be evicted.

Commits flush the transaction's dirty pages.

### Commit And Recovery

Frostfire uses two meta pages. Commits alternate between them:

1. write the new or modified data pages,
2. persist freelist changes,
3. write the next meta page naming the new roots,
4. sync the file.

Opening the database reads both meta pages and chooses the newest valid one. If
a crash happens in the middle of a commit, at least one previous meta page should
still name a complete committed tree.

There is no write-ahead log in this design. The copy-on-write tree plus
alternating meta pages provide the recovery boundary.

## Complete Example

```go
package main

import (
    "fmt"
    "log"

    "github.com/chaitanya-uike/frostfire"
)

func main() {
    db, err := frostfire.Open("example.db", frostfire.Options{})
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    // Write some data.
    w := db.BeginWrite()
    b, err := w.CreateBucket([]byte("greetings"))
    if err != nil {
        w.Abort()
        log.Fatal(err)
    }
    b.Put([]byte("en"), []byte("hello"))
    b.Put([]byte("fr"), []byte("bonjour"))
    b.Put([]byte("ja"), []byte("konnichiwa"))
    if err := w.Commit(); err != nil {
        log.Fatal(err)
    }

    // Read it back from a snapshot.
    r := db.BeginRead()
    defer r.Abort()
    b, _ = r.Bucket([]byte("greetings"))
    c := b.Cursor()
    defer c.Close()
    for k, v := c.First(); k != nil; k, v = c.Next() {
        fmt.Printf("%s: %s\n", k, v)
    }
}
```
