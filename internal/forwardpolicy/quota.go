// Package forwardpolicy keeps durable forward reservations for each Bridge login.
package forwardpolicy

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	limit    = 10
	database = "quota.db"
	bucket   = "reservations-v1"
)

var (
	ErrUnavailable = errors.New("forward policy unavailable")
	ErrLimit       = errors.New("forward quota reached")
)

// Initialize provisions a new owner-private state directory. An existing
// directory is never repaired or reused by initialization.
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
		_, err := tx.CreateBucket([]byte(bucket))
		return err
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
		closeErr := d.Close()
		if err != nil || closeErr != nil {
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

		key := sha256.Sum256([]byte(strings.ToLower(login)))
		record := b.Get(key[:])
		if record != nil && (len(record) == 0 || len(record)%8 != 0 || len(record) > 8*limit) {
			return ErrUnavailable
		}

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

		return b.Put(key[:], binary.BigEndian.AppendUint64(times, uint64(now.UnixNano())))
	})
}

func update(dir string, create bool, write func(*bolt.Tx) error) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return ErrUnavailable
	}
	if err := checkDirectory(dir, true); err != nil {
		return err
	}

	db, err := bolt.Open(filepath.Join(dir, database), 0600, &bolt.Options{
		Timeout: 5 * time.Second,
		OpenFile: func(path string, flags int, mode os.FileMode) (*os.File, error) {
			return openFile(path, create)
		},
	})
	if err != nil {
		return ErrUnavailable
	}

	err = db.Update(write)
	if closeErr := db.Close(); closeErr != nil || (err != nil && err != ErrLimit) {
		return ErrUnavailable
	}
	return err
}
