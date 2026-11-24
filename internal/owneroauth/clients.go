package owneroauth

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	bolt "go.etcd.io/bbolt"
)

var errUnknownClient = errors.New("unknown client ID")

type pairedClient struct {
	ID, Name string
}

func validClientID(id string) bool {
	if len(id) != 43 {
		return false
	}

	b, err := base64.RawURLEncoding.Strict().DecodeString(id)
	return err == nil && len(b) == 32
}

func listClients(db *bolt.DB, visit func(pairedClient) error) error {
	return db.View(func(tx *bolt.Tx) error {
		clients := tx.Bucket([]byte("clients"))
		if clients == nil {
			return nil
		}

		return clients.ForEach(func(id, name []byte) error {
			if !validClientID(string(id)) || name == nil || len(name) > maxClientNameBytes {
				return errState
			}
			return visit(pairedClient{ID: string(id), Name: string(name)})
		})
	})
}

func unpair(db *bolt.DB, id string) error {
	return db.Update(func(tx *bolt.Tx) error {
		clients := tx.Bucket([]byte("clients"))
		found := clients != nil && clients.Get([]byte(id)) != nil
		if found {
			if err := clients.Delete([]byte(id)); err != nil {
				return err
			}
		}

		families := tx.Bucket([]byte("families"))
		if families != nil {
			key := digest(id)
			if family := families.Bucket(key); family != nil {
				// Earlier bearers have no Client field. The family still
				// identifies its current bearer by hash.
				var state refreshFamily
				access := tx.Bucket([]byte("access"))
				if access == nil || json.Unmarshal(family.Get([]byte("current")), &state) != nil {
					return errState
				}
				if err := access.Delete(state.Access); err != nil {
					return err
				}
				if err := families.DeleteBucket(key); err != nil {
					return err
				}
				found = true
			}
		}

		for _, name := range []string{"deliveries", "codes", "access"} {
			b := tx.Bucket([]byte(name))
			if b == nil {
				return errState
			}

			c := b.Cursor()
			for k, v := c.First(); k != nil; {
				var record struct{ Client string }
				if json.Unmarshal(v, &record) != nil {
					return errState
				}
				if record.Client != id {
					k, v = c.Next()
					continue
				}

				if err := c.Delete(); err != nil {
					return err
				}
				found = true
				k, v = c.Seek(k)
			}
		}

		if !found {
			return errUnknownClient
		}
		return nil
	})
}

// Admin sends a local owner command or takes the exclusive store lock when
// there is no listener. A connected peer's denial is always final.
func Admin(ctx context.Context, dir, command, id string, tty io.Writer) error {
	request := command
	switch {
	case command == "clients" && id == "":
	case command == "unpair" && validClientID(id):
		request += " " + id
	default:
		return errInvalid
	}

	display := func(client pairedClient) error {
		_, err := fmt.Fprintf(tty, "%s\t%+q\n", client.ID, client.Name)
		return err
	}

	conn, err := sendOwnerRequest(ctx, dir, request)
	if errors.Is(err, errOffline) {
		db, err := openStore(dir, false)
		if err != nil {
			return errState
		}
		defer db.Close()

		if command == "clients" {
			err = listClients(db, display)
		} else {
			err = unpair(db, id)
		}
		if err != nil && !errors.Is(err, errUnknownClient) {
			return errState
		}
		return err
	}
	if err != nil {
		return err
	}
	defer conn.Close()

	// One bounded JSON line per client, followed by the final status.
	rows := bufio.NewScanner(conn)
	rows.Buffer(nil, maxOwnerReplyBytes)
	for rows.Scan() {
		var out reply
		if json.Unmarshal(rows.Bytes(), &out) != nil {
			return errState
		}
		if !out.OK {
			if out.UnknownClient {
				return errUnknownClient
			}
			return errState
		}
		if out.Client == nil {
			return nil
		}
		if command != "clients" || !validClientID(out.Client.ID) || len(out.Client.Name) > maxClientNameBytes {
			return errState
		}
		if err := display(*out.Client); err != nil {
			return err
		}
	}
	return errState
}
