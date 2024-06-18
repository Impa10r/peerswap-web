package db

import (
	"encoding/json"
	"fmt"
	"log"
	"path"
	"peerswap-web/cmd/psweb/config"

	"go.etcd.io/bbolt"
)

// Save saves any object to the Bolt database
func Save(bucketName string, key string, value interface{}) error {
	// Open the Bolt database
	db, err := bbolt.Open(path.Join(config.Config.DataDir, "psweb.db"), 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	return db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		if err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
}

// Load loads any object from the Bolt database
func Load(bucketName string, key string, result interface{}) error {
	// Open the Bolt database
	db, err := bbolt.Open(path.Join(config.Config.DataDir, "psweb.db"), 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	return db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket %s not found", bucketName)
		}
		data := b.Get([]byte(key))
		if data == nil {
			return fmt.Errorf("key %s not found in bucket %s", key, bucketName)
		}
		return json.Unmarshal(data, result)
	})
}
