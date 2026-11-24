// Package forwardpolicy keeps durable forward reservations and owner policy for each Bridge login.
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

	"github.com/wevial/croton-mcp/bridge"
	bolt "go.etcd.io/bbolt"
)

const (
	policyBucket = "forward-policy"
	bucket       = "reservations-v2"
	checksumKey  = "checksum"
)

var (
	ErrUnavailable    = errors.New("forward policy unavailable")
	ErrLimit          = errors.New("forward quota reached")
	ErrRecipientsFull = errors.New("remembered recipient list full")
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
func Reserve(cfg bridge.ForwardConfig, login, recipient string, confirmed bool) error {
	if !bridge.ValidForwardMailbox(recipient) {
		return ErrUnavailable
	}

	return withPolicy(cfg, login, func(policy *Policy, policies *bolt.Bucket) error {
		if !confirmed && !policy.Remembers(recipient) {
			return ErrUnavailable
		}

		b := policies.Tx().Bucket([]byte(bucket))
		key := accountKey(login)
		record := b.Get(key[:])

		now := time.Now()
		cutoff := now.Add(-24 * time.Hour).UnixNano()
		times := make([]byte, 0, len(record)+8)
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
		if len(times)/8 >= policy.DailyLimit {
			return ErrLimit
		}

		return b.Put(key[:], binary.BigEndian.AppendUint64(times, uint64(now.UnixNano())))
	})
}

func accountKey(login string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.ToLower(login)))
}

// Hash the complete, key-ordered set before trusting an absent login. Frame
// each fixed-size key and variable-length timestamp list unambiguously.
func stateChecksum(b *bolt.Bucket) ([]byte, error) {
	policies := b.Tx().Bucket([]byte(policyBucket))
	hash := sha256.New()
	err := b.ForEach(func(key, stamps []byte) error {
		if string(key) == checksumKey {
			return nil
		}
		if len(key) != sha256.Size || len(stamps) == 0 || len(stamps)%8 != 0 || len(stamps) > 8*bridge.MaxForwardDailyLimit {
			return ErrUnavailable
		}
		// A damaged branch key can hide an intact leaf from a login lookup.
		if !bytes.Equal(b.Get(key), stamps) {
			return ErrUnavailable
		}

		hash.Write(key)
		size := len(stamps)
		if policies != nil {
			size /= 8 // Up to 100 timestamps. The original format counted bytes.
		}
		hash.Write([]byte{byte(size)})
		hash.Write(stamps)
		return nil
	})
	reservations := hash.Sum(nil)
	if err != nil || policies == nil {
		return reservations, err // Existing quota-only databases retain their checksum.
	}

	hash.Reset()
	hash.Write(reservations)
	err = policies.ForEach(func(key, value []byte) error {
		if len(key) != sha256.Size || len(value) == 0 || !bytes.Equal(policies.Get(key), value) {
			return ErrUnavailable
		}

		hash.Write(key)
		hash.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
		hash.Write(value)
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

	// Keep other reservers out between closing the checked snapshot and
	// acquiring bbolt's writable lock, including across processes.
	lock, err := lockDirectory(dir)
	if err != nil {
		return err
	}
	defer lock.Close()

	path := filepath.Join(dir, "quota.db")
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	} else if err := quotaTransaction(path, os.O_RDONLY, checkStructure); err != nil {
		return err
	}

	return quotaTransaction(path, flags, write)
}

func checkStructure(tx *bolt.Tx) error {
	var err error
	// Check reports inconsistencies and recovered checker panics on its
	// channel. Drain it before the transaction releases the mapping.
	for range tx.Check() {
		err = ErrUnavailable
	}
	return err
}

func quotaTransaction(path string, flags int, run func(*bolt.Tx) error) (err error) {
	var file *os.File
	var db *bolt.DB
	defer func() {
		if recover() != nil {
			err = ErrUnavailable
		}
		if db != nil {
			if db.Close() != nil {
				err = ErrUnavailable
			}
		} else if file != nil {
			// Open can panic after it acquires the file lock.
			_ = file.Close()
		}
	}()

	readOnly := flags == os.O_RDONLY
	db, err = bolt.Open(path, 0600, &bolt.Options{
		Timeout:  5 * time.Second,
		ReadOnly: readOnly,
		OpenFile: func(path string, _ int, _ os.FileMode) (*os.File, error) {
			file, err = openFile(path, flags)
			return file, err
		},
	})
	if err != nil || !validMetaPages(file, db.Info().PageSize) {
		return ErrUnavailable
	}

	if readOnly {
		err = db.View(run)
	} else {
		err = db.Update(run)
	}
	if err != nil && err != ErrLimit && err != ErrRecipientsFull {
		return ErrUnavailable
	}
	return err
}

// bbolt's v2 file format has a 16-byte page header followed by 56 bytes
// of metadata and their FNV-1a checksum. Ordinary Open only needs one
// valid snapshot. Quota accounting needs both consecutive commits intact.
func validMetaPages(file *os.File, pageSize int) bool {
	info, err := file.Stat()
	if err != nil || pageSize <= 0 {
		return false
	}
	pages := uint64(info.Size() / int64(pageSize))
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
			order.Uint32(page[24:28]) != uint32(pageSize) || order.Uint64(page[56:64]) > pages || ids[i]%2 != uint64(i) ||
			order.Uint64(page[72:]) != checksum.Sum64() {
			return false
		}
	}
	return max(ids[0], ids[1])-min(ids[0], ids[1]) == 1
}
