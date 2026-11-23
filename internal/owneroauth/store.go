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
	PairingWindow   time.Duration
	HandoffLifetime time.Duration
	CodeLifetime    time.Duration
	AccessLifetime  time.Duration
	PendingLimit    int
	Now             func() time.Time
}

func Defaults() Settings {
	return Settings{
		PairingWindow:   3 * time.Minute,
		HandoffLifetime: 2 * time.Minute,
		CodeLifetime:    time.Minute,
		AccessLifetime:  10 * time.Minute,
		PendingLimit:    8,
		Now:             time.Now,
	}
}

type Authority struct {
	mu       sync.Mutex
	issuer   string
	settings Settings
	db       *bolt.DB
	window   window
	delivery delivery
}

func DefaultDir() (string, error) {
	if runtime.GOOS == "linux" {
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			if !filepath.IsAbs(dir) {
				return "", errState
			}
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
	if settings.Now == nil || settings.PendingLimit < 1 || settings.PairingWindow <= 0 || settings.HandoffLifetime <= 0 || settings.CodeLifetime <= 0 || settings.AccessLifetime < time.Second {
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
		for _, name := range []string{"codes", "access"} {
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
}

type access struct {
	Resource string
	Expires  time.Time
}

func put(tx *bolt.Tx, bucket, token string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Bucket([]byte(bucket)).Put(digest(token), data)
}

// Remove expired entries on issuance. Only terminal-approved grants can
// add records; rejected network requests never grow the database.
func prune(tx *bolt.Tx, now time.Time) error {
	for _, name := range []string{"codes", "access"} {
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
	return nil
}

func (a *Authority) redeem(code, client, redirect, resource, verifier string) (string, error) {
	if !validVerifier.MatchString(verifier) || resource != a.issuer+"/mcp" {
		return "", errDenied
	}

	token := secret()
	err := a.db.Update(func(tx *bolt.Tx) error {
		codes := tx.Bucket([]byte("codes"))
		key := digest(code)
		var g grant
		now := a.settings.Now()
		if json.Unmarshal(codes.Get(key), &g) != nil || !now.Before(g.Expires) || g.Client != client || g.Redirect != redirect || g.Resource != resource || !oauth2.IsPKCECodeVerifierMatch("S256", g.Challenge, verifier) {
			return errDenied
		}
		if err := codes.Delete(key); err != nil {
			return err
		}
		return put(tx, "access", token, access{resource, now.Add(a.settings.AccessLifetime)})
	})
	if err != nil {
		return "", errDenied
	}
	return token, nil
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
