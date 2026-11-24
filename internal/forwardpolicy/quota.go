// Package forwardpolicy keeps durable forward reservations for each Bridge login.
package forwardpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	limit       = 10
	bucket      = "reservations-v2"
	checksumKey = "checksum"
)

var (
	ErrUnavailable = errors.New("forward policy unavailable")
	ErrLimit       = errors.New("forward quota reached")
)

// Initialize creates owner-private quota state and refuses to reuse an existing directory.
func Initialize(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return ErrUnavailable
	}
	if err := checkDirectory(filepath.Dir(dir), false); err != nil {
		return err
	}

	if err := os.Mkdir(dir, 0700); err != nil {
		return ErrUnavailable
	}

	err := update(dir, true, func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte(bucket))
		if err != nil {
			return err
		}

		empty := sha256.Sum256(nil)
		return b.Put([]byte(checksumKey), empty[:])
	})
	if err != nil {
		return err
	}
	// Persist both new directory entries before a reservation can succeed.
	for _, path := range []string{dir, filepath.Dir(dir)} {
		d, err := os.Open(path)
		if err != nil {
			return ErrUnavailable
		}
		err = d.Sync()
		if closeErr := d.Close(); err != nil || closeErr != nil {
			return ErrUnavailable
		}
	}
	return nil
}

// Reserve commits a slot before any SMTP connection. There is no release on
// uncertain delivery. The bbolt file lock also serializes independent processes.
func Reserve(dir, login string) error {
	if login == "" {
		return ErrUnavailable
	}

	return update(dir, false, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return ErrUnavailable
		}
		checksum, err := reservationChecksum(b)
		if err != nil || !bytes.Equal(checksum, b.Get([]byte(checksumKey))) {
			return ErrUnavailable
		}

		key := sha256.Sum256([]byte(strings.ToLower(login)))
		record := b.Get(key[:])

		now := time.Now()
		cutoff := now.Add(-24 * time.Hour).UnixNano()
		times := make([]byte, 0, 8*limit)
		for len(record) != 0 {
			stamp := int64(binary.BigEndian.Uint64(record))
			if stamp <= 0 {
				return ErrUnavailable
			}
			if stamp > cutoff {
				times = append(times, record[:8]...)
			}
			record = record[8:]
		}
		if len(times) == 8*limit {
			return ErrLimit
		}

		if err := b.Put(key[:], binary.BigEndian.AppendUint64(times, uint64(now.UnixNano()))); err != nil {
			return err
		}
		checksum, err = reservationChecksum(b)
		if err != nil {
			return err
		}
		return b.Put([]byte(checksumKey), checksum)
	})
}

// Hash the complete, key-ordered set before trusting an absent login. Frame
// each fixed-size key and variable-length timestamp list unambiguously.
func reservationChecksum(b *bolt.Bucket) ([]byte, error) {
	hash := sha256.New()
	err := b.ForEach(func(key, stamps []byte) error {
		if string(key) == checksumKey {
			return nil
		}
		if len(key) != sha256.Size || len(stamps) == 0 || len(stamps)%8 != 0 || len(stamps) > 8*limit {
			return ErrUnavailable
		}
		// A damaged branch key can hide an intact leaf from a login lookup.
		if !bytes.Equal(b.Get(key), stamps) {
			return ErrUnavailable
		}

		hash.Write(key)
		hash.Write([]byte{byte(len(stamps))})
		hash.Write(stamps)
		return nil
	})
	return hash.Sum(nil), err
}

func update(dir string, create bool, write func(*bolt.Tx) error) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return ErrUnavailable
	}
	if err := checkDirectory(dir, true); err != nil {
		return err
	}

	var file *os.File
	db, err := bolt.Open(filepath.Join(dir, "quota.db"), 0600, &bolt.Options{
		Timeout: 5 * time.Second,
		OpenFile: func(path string, _ int, _ os.FileMode) (*os.File, error) {
			var err error
			file, err = openFile(path, create)
			return file, err
		},
	})
	if err != nil {
		return ErrUnavailable
	}

	// Check both snapshots under the exclusive lock before an update can hide a fallback.
	err = ErrUnavailable
	if validMetaPages(file, db.Info().PageSize) {
		err = db.Update(write)
	}
	if closeErr := db.Close(); closeErr != nil || (err != nil && err != ErrLimit) {
		return ErrUnavailable
	}
	return err
}

// bbolt's v2 file format has a 16-byte page header followed by 56 bytes
// of metadata and their FNV-1a checksum. Ordinary Open only needs one
// valid snapshot. Quota accounting needs both consecutive commits intact.
func validMetaPages(file *os.File, pageSize int) bool {
	var ids [2]uint64
	for i := range ids {
		var page [80]byte
		if _, err := file.ReadAt(page[:], int64(i)*int64(pageSize)); err != nil {
			return false
		}

		order := binary.NativeEndian
		ids[i] = order.Uint64(page[64:72])
		checksum := fnv.New64a()
		_, _ = checksum.Write(page[16:72])
		if order.Uint64(page[:8]) != uint64(i) || order.Uint16(page[8:10]) != 4 ||
			order.Uint32(page[16:20]) != 0xed0cdaed || order.Uint32(page[20:24]) != 2 ||
			order.Uint32(page[24:28]) != uint32(pageSize) || ids[i]%2 != uint64(i) ||
			order.Uint64(page[72:]) != checksum.Sum64() {
			return false
		}
	}
	return max(ids[0], ids[1])-min(ids[0], ids[1]) == 1
}
