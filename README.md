# Frostfire

Frostfire is an embedded, ACID-compliant key/value store written in Go. It runs in-process against a single file, exposing an transactional API over a copy-on-write B+tree.

Requirements: Go 1.25 or later, on Linux or macOS.

## Design

frostfire is inspired from [LMDB](http://www.lmdb.tech/doc/) and [boltdb](https://github.com/boltdb/bolt): a single-file embedded store, one writer at a time, lock-free snapshot reads, and data laid out as a copy-on-write B+tree of fixed-size pages.

### Buffer pool with direct I/O

LMDB and boltdb both rely on `mmap` and let the kernel page cache do the buffering. frostfire instead manages its own buffer pool in user space and reads/writes pages through direct I/O (`O_DIRECT` on Linux, `F_NOCACHE` on macOS), bypassing the page cache entirely.

### Copy-on-write B+tree

Updates never overwrite a live page. A write transaction copies any page it touches into a fresh page, propagates the new page id up to the root, and commits by atomically swapping the meta page that names the new root. Two meta pages alternate, so a crash mid-commit always leaves at least one intact root to recover from.

This gives readers their snapshots for free: a read transaction captures a meta page on entry and walks the tree rooted there. The writer can do whatever it likes to newer pages without disturbing it.

The result is no in-place updates, no write-ahead log, and no locks on the read path.

## Installation

```
go get github.com/chaitanya-uike/frostfire
```

## Opening a database

```go
db, err := frostfire.Open("data.db", frostfire.Options{})
if err != nil {
    return err
}
defer db.Close()
```

If the file does not exist it is created and initialised. If it exists, frostfire resumes from the last committed state.

`Options`:

| Field        | Default | Meaning                                                       |
| ------------ | ------- | ------------------------------------------------------------- |
| `BufferSize` | `1024`  | Number of 4 KiB page frames held in memory (~4 MiB at default). |

`db.Close()` flushes pending writes and releases the file.

## Transactions

All work happens inside a transaction. There are two kinds:

- **Read transactions** are obtained from `db.BeginRead()`. They observe a stable snapshot of the database and may run concurrently with other readers and with the writer. They cannot modify data.
- **Write transactions** are obtained from `db.BeginWrite()`. Only one write transaction may be active at a time; subsequent callers block until the current writer finishes.

Every transaction must be terminated, either with `Commit()` (write transactions only) or `Abort()`. Forgetting to do so leaks resources and, in the case of writers, deadlocks future writers.

```go
txn := db.BeginWrite()
defer txn.Abort() // safe no-op if Commit succeeded

// ... do work ...

if err := txn.Commit(); err != nil {
    return err
}
```

A commit is durable: when `Commit` returns nil, the data has been written and persisted. An abort discards all changes made by the transaction.

## Buckets

A bucket is a named, ordered key/value namespace. Buckets are created, looked up, and dropped through the transaction.

```go
txn := db.BeginWrite()
users, err := txn.CreateBucket([]byte("users"))
if err != nil {
    txn.Abort()
    return err
}
```

`CreateBucket` returns `ErrBucketExists` if the name is already in use. `txn.Bucket(name)` looks up an existing bucket and returns `ErrBucketNotFound` if it does not exist; it works in both read and write transactions. `txn.DropBucket(name)` removes a bucket and frees all pages it owns.

### Reading and writing keys

Within a bucket, four operations modify entries:

| Method                | Behaviour                                                              |
| --------------------- | ---------------------------------------------------------------------- |
| `Put(key, value)`     | Insert or replace. Always succeeds if the value fits.                  |
| `Insert(key, value)`  | Insert only. Returns `ErrKeyExists` if the key is already present.     |
| `Update(key, value)`  | Replace only. Returns `ErrKeyNotFound` if the key is absent.           |
| `Delete(key)`         | Remove the key. No error if the key was already absent.                |

`Get(key)` returns the value, or `(nil, nil)` if the key does not exist.

Keys are compared lexicographically as raw bytes. Encode keys in format which preserves lexicographical ordering

### Iteration

Each bucket exposes a cursor that walks its keys in sorted order:

```go
c := users.Cursor()
defer c.Close()

for k, v := c.First(); k != nil; k, v = c.Next() {
    fmt.Printf("%s = %s\n", k, v)
}
```

Cursor methods all return `(key, value)`, or `(nil, nil)` once iteration is exhausted:

- `First()` — first key in the bucket.
- `Last()` — last key in the bucket.
- `Seek(target)` — first key greater than or equal to `target`.
- `Next()` — advance one position.
- `Prev()` — step back one position.

Cursors hold pinned pages while open; call `Close()` when finished or the buffer pool will steadily fill.

## A complete example

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
