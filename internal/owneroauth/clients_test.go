//go:build linux || darwin

package owneroauth

import (
	"fmt"
	"slices"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestListClientsReleasesTransactionBeforeOutput(t *testing.T) {
	a := openTest(t, privateDir(t), Defaults())
	var want []pairedClient
	err := a.db.Update(func(tx *bolt.Tx) error {
		clients, err := tx.CreateBucket([]byte("clients"))
		if err != nil {
			return err
		}

		for i := range 150 {
			client := pairedClient{ID: fmt.Sprintf("%042dA", i), Name: fmt.Sprintf("client-%d.test", i)}
			if err := clients.Put([]byte(client.ID), []byte(client.Name)); err != nil {
				return err
			}
			want = append(want, client)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var got []pairedClient
	err = listClients(a.db, func(client pairedClient) error {
		if a.db.Stats().OpenTxN != 0 {
			t.Fatal("listing held a read transaction during output")
		}
		got = append(got, client)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatal("paged listing did not return every client in order")
	}
}
