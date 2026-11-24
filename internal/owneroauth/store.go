// Package owneroauth pairs one loopback MCP client with a local owner's approval.
package owneroauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"authelia.com/provider/oauth2"
	bolt "go.etcd.io/bbolt"
)

var (
	errState      = errors.New("owner state unavailable")
	errDenied     = errors.New("pairing request denied")
	validVerifier = regexp.MustCompile("^[A-Za-z0-9._~-]{43,128}$")
)

type Settings struct {
	PairingWindow      time.Duration
	HandoffLifetime    time.Duration
	CodeLifetime       time.Duration
	AccessLifetime     time.Duration
	RefreshIdleTimeout time.Duration
	PendingLimit       int
	Now                func() time.Time
}

func Defaults() Settings {
	return Settings{
		PairingWindow:      3 * time.Minute,
		HandoffLifetime:    2 * time.Minute,
		CodeLifetime:       time.Minute,
		AccessLifetime:     10 * time.Minute,
		RefreshIdleTimeout: 30 * 24 * time.Hour,
		PendingLimit:       8,
		Now:                time.Now,
	}
}

type Authority struct {
	mu       sync.Mutex
	issuer   string
	settings Settings
	db       *bolt.DB
	window   window
}

func DefaultDir() (string, error) {
	if runtime.GOOS == "linux" {
		if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
			return filepath.Join(dir, "croton-mcp"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errState
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "croton-mcp"), nil
	case "linux":
		return filepath.Join(home, ".local", "state", "croton-mcp"), nil
	}
	return "", errState
}

func Open(dir, issuer string, settings Settings) (*Authority, error) {
	if value, ok := os.LookupEnv("CROTON_REFRESH_IDLE_TIMEOUT"); ok {
		idle, err := time.ParseDuration(value)
		if err != nil {
			return nil, errState
		}
		settings.RefreshIdleTimeout = idle
	}

	if settings.Now == nil || settings.PendingLimit < 1 || settings.PairingWindow <= 0 || settings.HandoffLifetime <= 0 || settings.CodeLifetime <= 0 || settings.AccessLifetime < time.Second || settings.RefreshIdleTimeout <= 0 {
		return nil, errState
	}
	if err := secureDir(dir, true); err != nil {
		return nil, err
	}

	db, err := bolt.Open(filepath.Join(dir, "oauth.db"), 0600, &bolt.Options{Timeout: 100 * time.Millisecond, OpenFile: openDatabase})
	if err != nil {
		return nil, errState
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"codes", "access", "deliveries", "families"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, errState
	}
	return &Authority{issuer: issuer, settings: settings, db: db}, nil
}

func (a *Authority) Close() error { return a.db.Close() }

func secret() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

type grant struct {
	Client, Redirect, Challenge, Resource string
	Expires                               time.Time
	Refresh                               bool
}

type access struct {
	Resource string
	Expires  time.Time
}

func put(tx *bolt.Tx, bucket, token string, value any) error {
	return putRecord(tx.Bucket([]byte(bucket)), digest(token), value)
}

func putRecord(b *bolt.Bucket, key []byte, value any) error {
	if b == nil {
		return errState
	}

	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return b.Put(key, data)
}

// Reclaim expired grants when the owner approves a new one.
func prune(tx *bolt.Tx, now time.Time) error {
	for _, name := range []string{"codes", "access", "deliveries"} {
		b := tx.Bucket([]byte(name))
		c := b.Cursor()
		for k, v := c.First(); k != nil; {
			var record struct{ Expires time.Time }
			if json.Unmarshal(v, &record) != nil {
				return errState
			}
			if !now.Before(record.Expires) {
				if err := c.Delete(); err != nil {
					return err
				}
				k, v = c.Seek(k)
			} else {
				k, v = c.Next()
			}
		}
	}
	return pruneFamilies(tx, now)
}

func (a *Authority) redeem(code, client, redirect, resource, verifier string) (tokenPair, error) {
	if !validVerifier.MatchString(verifier) || resource != a.issuer+"/mcp" {
		return tokenPair{}, errDenied
	}

	pair := tokenPair{Access: secret()}
	err := a.db.Update(func(tx *bolt.Tx) error {
		codes := tx.Bucket([]byte("codes"))
		if codes == nil {
			return errState
		}

		key := digest(code)
		var g grant
		now := a.settings.Now()
		data := codes.Get(key)
		if data == nil {
			return errDenied
		}
		if err := json.Unmarshal(data, &g); err != nil {
			return errState
		}
		if !now.Before(g.Expires) || g.Client != client || g.Redirect != redirect || g.Resource != resource || !oauth2.IsPKCECodeVerifierMatch("S256", g.Challenge, verifier) {
			return errDenied
		}

		if err := codes.Delete(key); err != nil {
			return err
		}
		if g.Refresh && a.supportedGrantTypes().Has("refresh_token") {
			families := tx.Bucket([]byte("families"))
			if families == nil {
				return errState
			}
			family, err := families.CreateBucket(digest(client))
			if err != nil {
				return err
			}
			pair.Refresh = secret()
			state := refreshFamily{
				Resource: resource, Access: digest(pair.Access), Refresh: digest(pair.Refresh),
				Expires: now.Add(a.settings.RefreshIdleTimeout),
			}
			if err := putRecord(family, []byte("current"), state); err != nil {
				return err
			}
		}

		return put(tx, "access", pair.Access, access{resource, now.Add(a.settings.AccessLifetime)})
	})
	if errors.Is(err, errDenied) {
		return tokenPair{}, errDenied
	}
	if err != nil {
		return tokenPair{}, errState
	}

	return pair, nil
}

func (a *Authority) ValidBearer(token string) bool {
	return a.db.View(func(tx *bolt.Tx) error {
		var record access
		if json.Unmarshal(tx.Bucket([]byte("access")).Get(digest(token)), &record) != nil || record.Resource != a.issuer+"/mcp" || !a.settings.Now().Before(record.Expires) {
			return errDenied
		}
		return nil
	}) == nil
}
