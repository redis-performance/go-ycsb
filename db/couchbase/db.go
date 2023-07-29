package couchbase

import (
	"context"
	"fmt"
	"github.com/couchbase/gocb/v2"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"log"
	"time"
)

type dynamodbWrapper struct {
	client *gocb.Cluster
	bucket *gocb.Bucket
}

func (r *dynamodbWrapper) Close() error {
	return nil
}

func (r *dynamodbWrapper) InitThread(ctx context.Context, _ int, _ int) context.Context {
	return ctx
}

func (r *dynamodbWrapper) CleanupThread(_ context.Context) {
}

func (r *dynamodbWrapper) Read(ctx context.Context, table string, key string, fields []string) (data map[string][]byte, err error) {

	return

}

func (r *dynamodbWrapper) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	return nil, fmt.Errorf("scan is not supported")
}

func (r *dynamodbWrapper) Update(ctx context.Context, table string, key string, values map[string][]byte) (err error) {

	return
}

func (r *dynamodbWrapper) Insert(ctx context.Context, table string, key string, values map[string][]byte) (err error) {

	return
}

func (r *dynamodbWrapper) Delete(ctx context.Context, table string, key string) error {
	return nil
}

type dynamoDbCreator struct{}

func (r dynamoDbCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	rds := &dynamodbWrapper{}
	host := p.GetString(host, hostDefault)
	bucketName := p.GetString(bucket, bucketDefault)
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
	rds.client, err = gocb.Connect("couchbases://"+host, options)
	if err != nil {
		log.Fatal(err)
	}

	rds.bucket = rds.client.Bucket(bucketName)

	err = rds.bucket.WaitUntilReady(5*time.Second, nil)
	if err != nil {
		log.Fatal(err)
	}
	return rds, err
}

func (rds *dynamodbWrapper) deleteTable() error {

}

const (
	// The hostname from one server.
	host        = "couchbase.host"
	hostDefault = "127.0.0.1"

	// The bucket name to use.
	bucket        = "couchbase.bucket"
	bucketDefault = "default"

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
	kvDefault = false

	// The server parallelism for all n1ql queries.
	maxParallelism        = "couchbase.maxParallelism"
	maxParallelismDefault = 1

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
	ycsb.RegisterDBCreator("couchbase", dynamoDbCreator{})
}
