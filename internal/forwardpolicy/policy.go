package forwardpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"slices"
	"strings"

	"github.com/wevial/croton-mcp/bridge"
	bolt "go.etcd.io/bbolt"
)

// Policy is the account's effective local forward policy.
type Policy struct {
	DailyLimit           int
	RememberedRecipients []string
	Version              uint64 `json:"-"` // Transaction that observed this policy.
}

func (p Policy) Remembers(recipient string) bool {
	address, ok := bridge.CanonicalForwardMailbox(recipient)
	return ok && slices.Contains(p.RememberedRecipients, address)
}

func (p *Policy) allow(address string) error {
	if slices.Contains(p.RememberedRecipients, address) {
		return nil
	}
	if len(p.RememberedRecipients) >= bridge.MaxRememberedRecipients {
		return ErrRecipientsFull
	}

	p.RememberedRecipients = append(p.RememberedRecipients, address)
	return nil
}

// The quota and policy share one checksum, transaction and interprocess lock.
// The first policy operation on a login seeds only its owner-editable fields.
func withPolicy(cfg bridge.ForwardConfig, login string, change func(*Policy, *bolt.Bucket) error) error {
	if !bridge.ValidForwardMailbox(login) {
		return ErrUnavailable
	}

	return update(cfg.StateDir, false, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return ErrUnavailable
		}
		checksum, err := stateChecksum(b)
		if err != nil || !bytes.Equal(checksum, b.Get([]byte(checksumKey))) {
			return ErrUnavailable
		}

		policies, err := tx.CreateBucketIfNotExists([]byte(policyBucket))
		if err != nil {
			return err
		}
		key := accountKey(login)
		record := policies.Get(key[:])
		policy := Policy{DailyLimit: bridge.DefaultForwardDailyLimit, Version: uint64(tx.ID())}
		if record == nil {
			if cfg.DailyLimit != nil {
				policy.DailyLimit = *cfg.DailyLimit
			}
			for _, recipient := range cfg.RememberedRecipients {
				address, ok := bridge.CanonicalForwardMailbox(recipient)
				if !ok || policy.allow(address) != nil {
					return ErrUnavailable
				}
			}
		} else if err := json.Unmarshal(record, &policy); err != nil {
			return ErrUnavailable
		}
		if policy.DailyLimit < 1 || policy.DailyLimit > bridge.MaxForwardDailyLimit || len(policy.RememberedRecipients) > bridge.MaxRememberedRecipients {
			return ErrUnavailable
		}

		if err := change(&policy, policies); err != nil {
			return err
		}

		encoded, err := json.Marshal(policy)
		if err == nil && !bytes.Equal(encoded, record) {
			err = policies.Put(key[:], encoded)
		}
		if err != nil {
			return err
		}

		checksum, err = stateChecksum(b)
		if err == nil && !bytes.Equal(checksum, b.Get([]byte(checksumKey))) {
			err = b.Put([]byte(checksumKey), checksum)
		}
		return err
	})
}

// Only an owner forget writes this marker. Compare it to the transaction that
// issued the form so unrelated policy changes do not suppress learning.
func forgetKey(login, address string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.ToLower(login) + "\x00" + address))
}

func Inspect(cfg bridge.ForwardConfig, login string) (Policy, error) {
	var policy Policy
	err := withPolicy(cfg, login, func(p *Policy, _ *bolt.Bucket) error {
		policy = *p
		return nil
	})
	return policy, err
}

func SetLimit(cfg bridge.ForwardConfig, login string, limit int) error {
	if limit < 1 || limit > bridge.MaxForwardDailyLimit {
		return ErrUnavailable
	}

	return withPolicy(cfg, login, func(p *Policy, _ *bolt.Bucket) error {
		p.DailyLimit = limit
		return nil
	})
}

func Allow(cfg bridge.ForwardConfig, login, recipient string) error {
	address, ok := bridge.CanonicalForwardMailbox(recipient)
	if !ok {
		return ErrUnavailable
	}

	return withPolicy(cfg, login, func(p *Policy, _ *bolt.Bucket) error {
		return p.allow(address)
	})
}

func Forget(cfg bridge.ForwardConfig, login, recipient string) error {
	address, ok := bridge.CanonicalForwardMailbox(recipient)
	if !ok {
		return ErrUnavailable
	}

	return withPolicy(cfg, login, func(p *Policy, policies *bolt.Bucket) error {
		if i := slices.Index(p.RememberedRecipients, address); i >= 0 {
			p.RememberedRecipients = slices.Delete(p.RememberedRecipients, i, i+1)
		}

		key := forgetKey(login, address)
		return policies.Put(key[:], binary.BigEndian.AppendUint64(nil, p.Version))
	})
}

func Learn(cfg bridge.ForwardConfig, login, recipient string, version uint64) error {
	if !cfg.AutoRememberEnabled() {
		return nil
	}
	address, ok := bridge.CanonicalForwardMailbox(recipient)
	if !ok {
		return ErrUnavailable
	}

	return withPolicy(cfg, login, func(p *Policy, policies *bolt.Bucket) error {
		key := forgetKey(login, address)
		if value := policies.Get(key[:]); value != nil && binary.BigEndian.Uint64(value) > version {
			return nil
		}
		return p.allow(address)
	})
}
