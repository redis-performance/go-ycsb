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
const KEYSPACE_SEPARATOR = "."

type couchbaseWrapper struct {
	client            *gocb.Cluster
	bucket            *gocb.Bucket
	collection        *gocb.Collection
	bucketName        string
	scopeName         string
	collectionName    string
	scopeEnabled      bool
	collectionEnabled bool
	upsert            bool
	verbose           bool
}

/**
 * Helper function to generate the keyspace name.
 * @return a string with the computed keyspace name
 */
func (r *couchbaseWrapper) getKeyspaceName() (keyspaceName string) {
	if r.scopeEnabled || r.collectionEnabled {
		keyspaceName = r.bucketName + KEYSPACE_SEPARATOR + r.scopeName + KEYSPACE_SEPARATOR + r.collectionName
	} else {
		keyspaceName = r.bucketName
	}
	return
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

	return

}

func (r *couchbaseWrapper) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	return nil, fmt.Errorf("scan is not supported")
}

func (r *couchbaseWrapper) Update(ctx context.Context, table string, key string, values map[string][]byte) (err error) {

	return
}

func (r *couchbaseWrapper) Insert(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	id := table + KEY_SEPARATOR + key
	if r.upsert {
		_, err = r.collection.Upsert(id, values, nil)
		if err != nil {
			log.Fatalf("Error while upserting document with id %s and values %v", id, err)
		}
	} else {
		_, err = r.collection.Insert(id, values, nil)
		if err != nil {
			log.Fatalf("Error while inserting document with id %s and values %v", id, err)
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
	cb.bucketName = p.GetString(bucket, bucketDefault)
	cb.scopeName = p.GetString(scope, scopeDefault)
	cb.collectionName = p.GetString(collection, collectionDefault)
	cb.upsert = p.GetBool(upsert, upsertDefault)
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

	err = cb.client.WaitUntilReady(time.Second*3, nil)
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

	// The password of the bucket.
	username        = "couchbase.username"
	usernameDefault = ""

	// If mutations should wait for the response to complete.
	syncMutationResponse        = "couchbase.syncMutationResponse"
	syncMutationResponseDefault = true

	// Persistence durability requirement
	persistTo        = "couchbase.persistTo"
	persistToDefault = 0

	// Replication durability requirement
	replicateTo = "couchbase.replicateTo"

	replicateToDefault = 0

	// Use upsert instead of insert or replace.
	upsert        = "couchbase.upsert"
	upsertDefault = false

	// If set to true, prepared statements are not used.
	adhoc        = "couchbase.adhoc"
	adhocDefault = false

	// If set to false, mutation operations will also be performed through N1QL.
	kv        = "couchbase.kv"
	kvDefault = true

	// The server parallelism for all n1ql queries.
	maxParallelism        = "couchbase.maxParallelism"
	maxParallelismDefault = 0

	// The number of KV sockets to open per server.
	kvEndpoints        = "couchbase.kvEndpoints"
	kvEndpointsDefault = 1

	// The number of N1QL Query sockets to open per server.
	queryEndpoints        = "couchbase.queryEndpoints"
	queryEndpointsDefault = 5

	// If Epoll instead of NIO should be used (only available for linux.
	epoll        = "couchbase.epoll"
	epollDefault = false

	// If > 0 trades CPU for higher throughput. N is the number of event loops,
	// ideally set to the number of physical cores.
	// Setting higher than that will likely degrade performance.
	boost        = "couchbase.boost"
	boostDefault = 3

	// The interval in seconds when latency metrics will be logged.
	networkMetricsInterval        = "couchbase.networkMetricsInterval"
	networkMetricsIntervalDefault = 0

	// The interval in seconds when runtime metrics will be logged.
	runtimeMetricsInterval        = "couchbase.runtimeMetricsInterval"
	runtimeMetricsIntervalDefault = 0

	// Document Expiry is the amount of time(second) until a document expires in Couchbase.
	documentExpiry        = "couchbase.documentExpiry"
	documentExpiryDefault = 0
)

func init() {
	ycsb.RegisterDBCreator("couchbase", couchbaseCreator{})
}
