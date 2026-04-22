package utils

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/binary"
	"sync"
	"time"
	"unsafe"

	"github.com/gofrs/uuid/v5"
	"github.com/metacubex/randv2"
)

func UnsafeRandRead(p []byte) {
	for len(p) > 0 {
		v := randv2.Uint64()
		if v == 0 {
			continue
		}
		i := copy(p, (*[8]byte)(unsafe.Pointer(&v))[:])
		p = p[i:]
	}
}

type unsafeRandReader struct{}

func (r *unsafeRandReader) Read(p []byte) (n int, err error) {
	UnsafeRandRead(p)
	return len(p), nil
}

var UnsafeRandReader = (*unsafeRandReader)(nil)

// NewUUIDV3 returns a UUID based on the MD5 hash of the namespace UUID and name.
func NewUUIDV3(ns uuid.UUID, name string) (u uuid.UUID) {
	h := md5.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	copy(u[:], h.Sum(make([]byte, 0, md5.Size)))

	u.SetVersion(uuid.V3)
	u.SetVariant(uuid.VariantRFC9562)
	return u
}

// NewUUIDV4 returns a new version 4 UUID.
//
// Version 4 UUIDs contain 122 bits of random data.
func NewUUIDV4() (u uuid.UUID) {
	UnsafeRandRead(u[:])
	u.SetVersion(uuid.V4)
	u.SetVariant(uuid.VariantRFC9562)
	return u
}

// NewUUIDV5 returns a UUID based on SHA-1 hash of the namespace UUID and name.
func NewUUIDV5(ns uuid.UUID, name string) (u uuid.UUID) {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	copy(u[:], h.Sum(make([]byte, 0, sha1.Size)))

	u.SetVersion(uuid.V5)
	u.SetVariant(uuid.VariantRFC9562)
	return u
}

var (
	v7mu            sync.Mutex
	v7lastSecs      uint64
	v7lastTimestamp uint64
)

// NewUUIDV7 returns a new version 7 UUID.
//
// Version 7 UUIDs contain a timestamp in the most significant 48 bits,
// and at least 62 bits of random data.
//
// NewUUIDV7 always returns UUIDs which sort in increasing order,
// except when the system clock moves backwards.
func NewUUIDV7() (u uuid.UUID) {
	// UUIDv7 is defined in RFC 9562 section 5.7 as:
	//
	//  0                   1                   2                   3
	//  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	// |                           unix_ts_ms                          |
	// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	// |          unix_ts_ms           |  ver  |       rand_a          |
	// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	// |var|                        rand_b                             |
	// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	// |                            rand_b                             |
	// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//
	// We store a 12 bit sub-millisecond timestamp fraction in the rand_a section,
	// as optionally permitted by the RFC.
	v7mu.Lock()

	// Generate our 60-bit timestamp: 48 bits of millisecond-resolution,
	// followed by 12 bits of 1/4096-millisecond resolution.
	now := time.Now()
	secs := uint64(now.Unix())
	nanos := uint64(now.Nanosecond())
	msecs := nanos / 1000000
	frac := nanos - (1000000 * msecs)
	timestamp := (1000*secs + msecs) << 12 // ms shifted into position
	timestamp += (frac * 4096) / 1000000   // ns converted to 1/4096-ms units

	if v7lastSecs > secs {
		// Time has gone backwards.
		// This presumably indicates the system clock has changed.
		// Ignore previously-generated UUIDs.
	} else if timestamp <= v7lastTimestamp {
		// This timestamp is the same as a previously-generated UUID.
		// To preserve the property that we generate UUIDs in order,
		// use a timestamp 1/4096 millisecond later than the most recently
		// generated UUID.
		timestamp = v7lastTimestamp + 1
	}

	v7lastSecs = secs
	v7lastTimestamp = timestamp
	v7mu.Unlock()

	// Insert a gap for the 4 bits of the ver field into the timestamp.
	hibits := ((timestamp << 4) & 0xffff_ffff_ffff_0000) | (timestamp & 0x0ffff)

	binary.BigEndian.PutUint64(u[0:8], hibits)
	UnsafeRandRead(u[8:])

	u.SetVersion(uuid.V7)
	u.SetVariant(uuid.VariantRFC9562)
	return u
}

// UUIDMap https://github.com/XTLS/Xray-core/issues/158#issue-783294090
func UUIDMap(str string) uuid.UUID {
	u, err := uuid.FromString(str)
	if err != nil {
		return NewUUIDV5(uuid.Nil, str)
	}
	return u
}
