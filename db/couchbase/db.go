package couchbase

import (
	"context"
	"fmt"
	gocb "github.com/couchbase/gocb/v2"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"log"
	"time"
)

const KEY_SEPARATOR = ":"

type couchbaseWrapper struct {
	client            *gocb.Cluster
	bucket            *gocb.Bucket
	collection        *gocb.Collection
	bucketName        string
	scopeName         string
	collectionName    string
	persistTo         uint
	replicateTo       uint
	scopeEnabled      bool
	collectionEnabled bool
	upsert            bool
	upsertOptions     *gocb.UpsertOptions
	insertOptions     *gocb.InsertOptions
	replaceOptions    *gocb.ReplaceOptions
	getOptions        *gocb.GetOptions
	kvTimeout         time.Duration
	verbose           bool
}

func (r *couchbaseWrapper) Close() (err error) {
	return
}

func (r *couchbaseWrapper) InitThread(ctx context.Context, _ int, _ int) context.Context {
	return ctx
}

func (r *couchbaseWrapper) CleanupThread(_ context.Context) {
}

func (r *couchbaseWrapper) Read(ctx context.Context, table string, key string, fields []string) (data map[string][]byte, err error) {
	id := getCouchbaseId(table, key)
	getResult, err := r.collection.Get(id, r.getOptions)
	if err != nil {
		log.Fatal(err)
	}

	err = getResult.Content(&data)
	if err != nil {
		log.Fatal(err)
	}
	return

}

func (r *couchbaseWrapper) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	return nil, fmt.Errorf("scan is not supported")
}

func (r *couchbaseWrapper) Update(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	id := getCouchbaseId(table, key)
	_, err = r.collection.Replace(id, values, r.replaceOptions)
	if err != nil {
		log.Fatalf("Error while updating document with id %s. Error message: %v", id, err)
	}
	return
}

func getCouchbaseId(table string, key string) string {
	id := table + KEY_SEPARATOR + key
	return id
}

func (r *couchbaseWrapper) Insert(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	id := getCouchbaseId(table, key)
	if r.upsert {
		_, err = r.collection.Upsert(id, values, r.upsertOptions)
		if err != nil {
			log.Fatalf("Error while upserting document with id %s. Error message: %v", id, err)
		}
	} else {
		_, err = r.collection.Insert(id, values, r.insertOptions)
		if err != nil {
			log.Fatalf("Error while inserting document with id %s. Error message: %v", id, err)
		}
	}
	return
}

func (r *couchbaseWrapper) Delete(ctx context.Context, table string, key string) error {
	return nil
}

type couchbaseCreator struct{}

func (r couchbaseCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	cb := &couchbaseWrapper{}
	cb.verbose = p.GetBool(prop.Verbose, prop.VerboseDefault)
	if cb.verbose {
		gocb.SetLogger(gocb.DefaultStdioLogger())
	}
	host := p.GetString(host, hostDefault)
	documentExpiry := time.Second * time.Duration(p.GetInt64(documentExpiry, documentExpiryDefault))
	cb.kvTimeout = time.Millisecond * time.Duration(p.GetInt64(kvTimeout, kvTimeoutDefault))
	cb.bucketName = p.GetString(bucket, bucketDefault)
	cb.scopeName = p.GetString(scope, scopeDefault)
	cb.collectionName = p.GetString(collection, collectionDefault)
	cb.persistTo = p.GetUint(persistTo, persistToDefault)
	cb.replicateTo = p.GetUint(replicateTo, replicateToDefault)
	cb.upsert = p.GetBool(upsert, upsertDefault)
	if cb.upsert {
		cb.upsertOptions = &gocb.UpsertOptions{
			PersistTo:   cb.persistTo,
			ReplicateTo: cb.replicateTo,
			Expiry:      documentExpiry,
			Timeout:     cb.kvTimeout,
		}
	} else {
		cb.insertOptions = &gocb.InsertOptions{
			PersistTo:   cb.persistTo,
			ReplicateTo: cb.replicateTo,
			Expiry:      documentExpiry,
			Timeout:     cb.kvTimeout,
		}
	}
	cb.replaceOptions = &gocb.ReplaceOptions{
		PersistTo:   cb.persistTo,
		ReplicateTo: cb.replicateTo,
		Expiry:      documentExpiry,
		Timeout:     cb.kvTimeout,
	}
	cb.getOptions = &gocb.GetOptions{
		Timeout: cb.kvTimeout,
	}
	username := p.GetString(username, usernameDefault)
	password := p.GetString(password, passwordDefault)

	options := gocb.ClusterOptions{
		Authenticator: gocb.PasswordAuthenticator{
			Username: username,
			Password: password,
		},
	}
	var err error = nil
	// Sets a pre-configured profile called "wan-development" to help avoid latency issues
	// when accessing Capella from a different Wide Area Network
	// or Availability Zone (e.g. your laptop).
	err = options.ApplyProfile(gocb.ClusterConfigProfileWanDevelopment)
	if err != nil {
		log.Fatal(err)
	}

	// Initialize the Connection
	cb.client, err = gocb.Connect("couchbases://"+host, options)
	if err != nil {
		log.Fatalf("Error while connecting to %s. Error: %s", host, err.Error())
	}

	err = cb.client.WaitUntilReady(5*time.Second, nil)
	if err != nil {
		log.Fatalf("Error while waiting until ready: %s", err.Error())
	}
	cb.bucket = cb.client.Bucket(cb.bucketName)
	err = cb.bucket.WaitUntilReady(5*time.Second, nil)
	if err != nil {
		log.Fatalf("Error while waiting for bucket: %s", err.Error())
	}

	cb.collection = cb.bucket.Scope(cb.scopeName).Collection(cb.collectionName)
	pingres, err := cb.client.Ping(nil)
	if err != nil {
		log.Fatalf("Error while ping: %s", err.Error())
	} else {
		jsondata, _ := pingres.MarshalJSON()
		log.Printf("Couchbase PING result: %s", jsondata)
	}

	return cb, err
}

func (cb *couchbaseWrapper) deleteTable() (err error) {
	return
}

const (
	// The hostname from one server.
	host        = "couchbase.host"
	hostDefault = "127.0.0.1"

	// The bucket name to use.
	bucket        = "couchbase.bucket"
	bucketDefault = "ycsb"

	// The scope to use.
	scope        = "couchbase.scope"
	scopeDefault = "_default"

	// The collection to use.
	collection        = "couchbase.collection"
	collectionDefault = "_default"

	// The password of the bucket.
	password        = "couchbase.password"
	passwordDefault = ""

	// The username of the bucket.
	username        = "couchbase.username"
	usernameDefault = ""

	// Persistence durability requirement
	persistTo        = "couchbase.persistTo"
	persistToDefault = 0

	// Replication durability requirement
	replicateTo = "couchbase.replicateTo"

	replicateToDefault = 0

	// Use upsert instead of insert or replace.
	upsert        = "couchbase.upsert"
	upsertDefault = false

	// Amount of time(second) until a document expires in Couchbase
	documentExpiry        = "couchbase.documentExpiry"
	documentExpiryDefault = 0

	// Upsert/Insert/Read operations timeout in milliseconds
	kvTimeout        = "couchbase.kvTimeout"
	kvTimeoutDefault = 2000
)

func init() {
	ycsb.RegisterDBCreator("couchbase", couchbaseCreator{})
}
