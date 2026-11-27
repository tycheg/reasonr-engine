package memory

import (
	"encoding/binary"
	"hash/fnv"
	"time"

	"github.com/tycheg/reasonr-engine/internal/storekit"
)

// Digest is a hash of the store state: rows, events, timers, external
// requests, responses by key and counters. The sum of the hashes of the
// parts modulo 2⁶⁴ does not depend on the walk order: two copies of a store
// that ran the same units of work in the same order get one Digest.
func (s *Store) Digest() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := part("position", uint64(s.position), s.timerSeq)
	for tenant, sp := range s.spaces {
		t := string(tenant)
		for typ, rows := range sp.rows {
			for key, r := range rows {
				x := part("row", t, uint64(typ), key, r.version, r.data, sealedBytes(r.sealed))
				for _, e := range r.events {
					x += part("event", t, key, uint64(e.Position), uint64(e.Name), e.Args, sealedBytes(e.Sealed))
				}
				h += x
			}
		}
		for id, tm := range sp.timers {
			h += part("timer", t, id[:], tm.at, uint64(tm.offset), tm.order, uint64(tm.entity), uint64(tm.command),
				tm.sealed, tm.leaseUntil, uint64(tm.attempts), tm.dead)
		}
		for key, rq := range sp.requests {
			h += part("request", t, key[:], uint64(rq.position), uint64(rq.index), rq.next, uint64(rq.attempts), rq.last)
		}
		for key, k := range sp.keys {
			h += part("key", t, key, k.data, k.expires)
		}
		for typ, n := range sp.seqs {
			h += part("seq", t, uint64(typ), uint64(n))
		}
		h += part("applied", t, uint64(sp.applied))
	}
	return h
}

// part is the hash of one part of the state from strings, bytes, numbers and moments.
func part(items ...any) uint64 {
	f := fnv.New64a()
	var b [8]byte
	for _, it := range items {
		switch v := it.(type) {
		case string:
			binary.BigEndian.PutUint64(b[:], uint64(len(v)))
			f.Write(b[:])
			f.Write([]byte(v))
		case []byte:
			binary.BigEndian.PutUint64(b[:], uint64(len(v)))
			f.Write(b[:])
			f.Write(v)
		case uint64:
			binary.BigEndian.PutUint64(b[:], v)
			f.Write(b[:])
		case time.Time:
			binary.BigEndian.PutUint64(b[:], uint64(v.UnixNano()))
			f.Write(b[:])
		}
	}
	return f.Sum64()
}

func sealedBytes(ss []storekit.Sealed) []byte {
	var out []byte
	for _, s := range ss {
		out = binary.BigEndian.AppendUint64(out, uint64(s.Field))
		out = append(out, s.Data...)
	}
	return out
}
