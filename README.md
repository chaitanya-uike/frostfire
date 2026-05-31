# Frostfire

Frostfire is an embedded, ACID-compliant key/value store written in Go. It runs
in-process against a single file and exposes a transactional API over a
copy-on-write B+tree.

Requirements: Go 1.25 or later, on Linux or macOS.

## Installation

```sh
go get github.com/chaitanya-uike/frostfire
```

Minimal write/read flow:

```go
db, err := frostfire.Open("data.db", frostfire.Options{})
if err != nil {
    return err
}
defer db.Close()

w := db.BeginWrite()
defer w.Abort()

bucket, err := w.CreateBucket([]byte("users"))
if err != nil {
    return err
}
if err := bucket.Put([]byte("alice"), []byte("admin")); err != nil {
    return err
}
if err := w.Commit(); err != nil {
    return err
}

r := db.BeginRead()
defer r.Abort()

bucket, err = r.Bucket([]byte("users"))
if err != nil {
    return err
}
value, err := bucket.Get([]byte("alice"))
if err != nil {
    return err
}
```

## Opening A Database

`Open` creates a database file if it does not exist, or resumes from the last
committed state if it does.

```go
db, err := frostfire.Open("data.db", frostfire.Options{})
if err != nil {
    return err
}
defer db.Close()
```

Options:

| Field | Default | Meaning |
| --- | --- | --- |
| `BufferSize` | `1024` | Number of 4 KiB page frames kept in the buffer pool. |

At the default buffer size, Frostfire keeps about 4 MiB of page frames in
memory. `db.Close()` flushes and releases the underlying file resources.

## Transactions

All work in Frostfire happens inside a transaction.

Frostfire allows **many concurrent readers** and **one writer**:

- `BeginRead()` opens a read-only snapshot. It can run concurrently with other
  readers and with the active writer.
- `BeginWrite()` opens a write transaction. Only one write transaction can be
  active at a time; other writers block until the current writer commits or
  aborts.

Every transaction must end with `Commit()` or `Abort()`. Read transactions are
ended with `Abort()`. Write transactions use `Commit()` to publish changes or
`Abort()` to discard them.

```go
tx := db.BeginWrite()
defer tx.Abort() // safe no-op if Commit succeeds

// ... mutate buckets ...

if err := tx.Commit(); err != nil {
    return err
}
```

When `Commit()` returns `nil`, the transaction's dirty pages and new meta page
have been written and synced. `Abort()` discards changes made by the
transaction.

### Why Batch Writes

Each write transaction pays a commit cost: dirty pages are flushed, metadata is
updated, and the database file is synced for durability. Batching related writes
into one transaction pays that cost once instead of once per item.

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

### Transaction Lifetime

Transactions should be short-lived and owned by one goroutine.

- Use `defer tx.Abort()` after `BeginWrite()` so early returns release the
  writer slot.
- Do not hold a write transaction while doing network I/O, waiting for user
  input, sleeping, or doing unrelated work.
- Do not pass a `Txn`, `Bucket`, or `Cursor` between goroutines.
- Close read transactions when finished. Long-lived readers keep their snapshot
  alive and can delay page reuse.

## Buckets

A bucket is a named, ordered key/value namespace. Bucket names are byte slices,
and bucket operations happen through a transaction.

```go
tx := db.BeginWrite()
defer tx.Abort()

users, err := tx.CreateBucket([]byte("users"))
if err != nil {
    return err
}

if err := tx.Commit(); err != nil {
    return err
}
```

Bucket management:

| Method | Behavior |
| --- | --- |
| `CreateBucket(name)` | Creates a bucket, or returns `ErrBucketExists`. |
| `Bucket(name)` | Looks up a bucket, or returns `ErrBucketNotFound`. Works in read and write transactions. |
| `DropBucket(name)` | Removes a bucket and frees the pages it owns. Write transactions only. |

### Reading And Writing Keys

Within a bucket, keys and values are raw byte slices. Keys are ordered
lexicographically as bytes. If you encode numbers, timestamps, or compound keys,
choose an encoding whose byte order matches the sort order you want.

| Method | Behavior |
| --- | --- |
| `Get(key)` | Returns the value, or `(nil, nil)` if the key does not exist. |
| `Put(key, value)` | Inserts or replaces a key. |
| `Insert(key, value)` | Inserts only, returning `ErrKeyExists` if the key is present. |
| `Update(key, value)` | Replaces only, returning `ErrKeyNotFound` if the key is absent. |
| `Delete(key)` | Removes a key. Deleting a missing key is not an error. |

Write methods require a writable transaction. Calling them through a read
transaction returns `ErrTxnReadOnly`.

### Bulk Loading

`BeginBulkLoad` builds an empty B+tree bottom-up from sorted input. It is useful
when initially populating a bucket because it avoids the copy-on-write cost of
calling `Put` repeatedly.

```go
loader, err := bucket.BeginBulkLoad(frostfire.BulkLoadOptions{FillFactor: 0.7})
if err != nil {
    return err
}
for _, item := range sortedItems {
    if err := loader.Append(item.Key, item.Value); err != nil {
        loader.Abort()
        return err
    }
}
if _, err := loader.Finish(); err != nil {
    return err
}
```

Contract:

- The bucket's B+tree must be empty, or `BeginBulkLoad` returns
  `ErrBTreeNotEmpty`.
- Keys passed to `Append` must be strictly increasing, or `Append` returns
  `ErrBulkLoaderKeyOrder`.
- Call `Finish()` to publish the tree or `Abort()` to discard it.
- After `Finish()` or `Abort()`, the `BulkLoader` is closed.
- Keep bucket access in one goroutine while bulk loading.

`BulkLoadOptions.FillFactor` controls how full pages are packed before rolling
to a sibling. The default `0` means `0.7`. Values are clamped to `[0.3, 1.0]`.
Lower fill factors leave more room for later random writes; `1.0` packs pages
as densely as possible.

### Cursors

Each bucket exposes a cursor that walks its keys in sorted order:

```go
c := users.Cursor()
defer c.Close()

for k, v := c.First(); k != nil; k, v = c.Next() {
    fmt.Printf("%s = %s\n", k, v)
}
```

Cursor methods return `(key, value)`, or `(nil, nil)` when there is no current
entry.

| Method | Behavior |
| --- | --- |
| `First()` | Move to the smallest key. |
| `Last()` | Move to the largest key. |
| `Seek(target)` | Move to the first key greater than or equal to `target`. |
| `Next()` | Advance one key. |
| `Prev()` | Move back one key. |

Cursors pin pages while they are open. Call `Close()` when finished so the
buffer pool can evict those pages later.

## How It Works

Frostfire is inspired by [LMDB](http://www.lmdb.tech/doc/) and
[BoltDB](https://github.com/boltdb/bolt): a single-file embedded store, one
writer at a time, snapshot reads, and ordered keys stored in copy-on-write
B+trees over fixed-size pages.

### Mental Model

The central idea is copy-on-write. A writer never overwrites pages that an
active reader might still be using. When an update changes a leaf, splits a
page, merges pages, or changes child pointers, Frostfire writes the changed
version into another page and leaves the old page in place. The transaction is
published by writing a meta page that names the new roots. Readers keep walking
the roots they saw when their transaction began.

- Readers get stable snapshots.
- One writer builds the next version of the tree.
- Old pages remain valid until no active reader can still see them.
- Commit publishes the new version durably.

### Copy-On-Write B+tree

Each bucket is stored as a B+tree. Leaf pages contain keys and values. Internal
pages contain separator keys and child page IDs.

Updating a key copies the leaf that contains it. If the copied leaf splits, a
separator key is pushed into the parent. If the parent must change, it is copied
too. This continues up to the root, producing a new root page for the
transaction.

Deletes follow the same copy-on-write rule. They copy the affected child, then
repair underfull pages by borrowing from siblings or merging pages. Parent
pages are copied as their child pointers and separator keys change.

The old pages are not overwritten. They remain valid for any read transaction
that started before the writer committed.

`BeginBulkLoad` uses a different path for initial population. Because it
requires an empty B+tree and sorted input, Frostfire can pack fresh leaf pages
directly and build the upper levels from them. The result is still a normal
Frostfire B+tree; it is just built bottom-up instead of through repeated
copy-on-write updates.

### Snapshots And Page Reuse

A read transaction captures the current meta page when it begins. That meta page
names the catalog root, freelist root, page count, and transaction ID. Reads
walk only the pages reachable from that snapshot.

When a write transaction frees a page, Frostfire cannot always reuse it
immediately. An older reader may still be able to reach that page from its
snapshot. Frostfire records freed pages with the transaction that freed them,
then makes those pages reusable only after active readers are new enough.

Long-lived read transactions are therefore cheap for concurrency, but not free:
they can keep old pages alive and delay reuse.

### Buffer Pool And Direct I/O

Unlike LMDB and BoltDB, Frostfire does not use `mmap` for page access. It
manages its own buffer pool and reads/writes pages through direct I/O
(`O_DIRECT` on Linux, `F_NOCACHE` on macOS), bypassing the OS page cache.

The buffer pool keeps a fixed number of page frames in memory. Pages are pinned
while code is using them, dirty when modified, and evicted with a clock-style
replacement policy when space is needed. Pinned pages cannot be evicted.

Commits flush the transaction's dirty pages.

### Commit And Recovery

Frostfire commits by publishing a new version of the database, not by modifying
the old version in place. The data pages for the new version are written first.
Only after those pages are on disk does Frostfire write the meta page that names
the new roots.

There are two meta pages, and commits alternate between them. Opening the
database means reading both meta pages and choosing the newest valid one. If a
crash happens halfway through a commit, the previous meta page still points at
the last complete version of the database.

This design does not need a write-ahead log. The copy-on-write tree preserves
the old version while the new one is being built, and the meta page switch is
the recovery boundary.

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
    defer w.Abort()

    b, err := w.CreateBucket([]byte("greetings"))
    if err != nil {
        log.Fatal(err)
    }
    if err := b.Put([]byte("en"), []byte("hello")); err != nil {
        log.Fatal(err)
    }
    if err := b.Put([]byte("fr"), []byte("bonjour")); err != nil {
        log.Fatal(err)
    }
    if err := b.Put([]byte("ja"), []byte("konnichiwa")); err != nil {
        log.Fatal(err)
    }
    if err := w.Commit(); err != nil {
        log.Fatal(err)
    }

    // Read it back from a snapshot.
    r := db.BeginRead()
    defer r.Abort()

    b, err = r.Bucket([]byte("greetings"))
    if err != nil {
        log.Fatal(err)
    }
    c := b.Cursor()
    defer c.Close()
    for k, v := c.First(); k != nil; k, v = c.Next() {
        fmt.Printf("%s: %s\n", k, v)
    }
}
```
