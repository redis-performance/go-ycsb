// Copyright (c) 2020 Daimler TSS GmbH TLS support

package mongodb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"io/ioutil"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

const (
	mongodbUrl      = "mongodb.url"
	mongodbAuthdb   = "mongodb.authdb"
	mongodbUsername = "mongodb.username"
	mongodbPassword = "mongodb.password"

	// see https://github.com/brianfrankcooper/YCSB/tree/master/mongodb#mongodb-configuration-parameters
	mongodbUrlDefault      = "mongodb://127.0.0.1:27017/ycsb?w=1"
	mongodbDatabaseDefault = "ycsb"
	mongodbAuthdbDefault   = "admin"
	mongodbTLSSkipVerify   = "mongodb.tls_skip_verify"
	mongodbTLSCAFile       = "mongodb.tls_ca_file"

	// mongodbWriteConcern: "majority" or a numeric ack count (e.g. "1", "2").
	// mongodbWriteConcernJournal: also require the write to hit the on-disk journal.
	// Together, e.g. mongodb.write_concern=majority mongodb.write_concern_journal=true,
	// these give durable/synchronous writes instead of the mongodbUrlDefault's w=1.
	mongodbWriteConcern        = "mongodb.write_concern"
	mongodbWriteConcernJournal = "mongodb.write_concern_journal"
	// mongodbReadConcern: "local", "available", "majority", or "linearizable"
	// - the consistency/durability guarantee of data returned by a read (e.g.
	// mongodb.read_concern=majority to only read majority-committed data).
	// "snapshot" is not offered: it requires a multi-document
	// transaction/session, which this adapter's Read/Scan never open.
	mongodbReadConcern = "mongodb.read_concern"
	// mongodbReadPreference: "primary", "primaryPreferred", "secondary",
	// "secondaryPreferred", or "nearest" - which replica-set member(s) reads
	// are routed to. This is a distinct knob from mongodbReadConcern: read
	// preference picks WHERE a read goes, read concern picks HOW committed
	// the data it returns must be.
	mongodbReadPreference = "mongodb.read_preference"
	// mongodbWriteConcernTimeout (wtimeout): how long the server waits for
	// the configured write concern (e.g. majority) to be satisfied before
	// giving up. Without it, mongodb.write_concern=majority against a
	// replica set that can't currently reach a majority (a down/partitioned
	// secondary) blocks the calling goroutine indefinitely - there is no
	// other deadline anywhere in the request path.
	mongodbWriteConcernTimeout = "mongodb.write_concern_timeout"
	// mongodbSocketTimeout bounds every socket read/write, so a connection
	// that goes quiet without closing (a NAT/security-group idle-connection
	// drop, a network partition, a paused VM) fails the operation instead of
	// hanging the worker goroutine forever.
	mongodbSocketTimeout = "mongodb.socket_timeout"
)

// mongoDatabaseNameRe rejects the characters MongoDB forbids in a database
// name (see https://www.mongodb.com/docs/manual/reference/limits/#naming-restrictions),
// so a malformed mongodb.url path segment (a stray "//", an unescaped space
// or dot) is caught here instead of surfacing as an opaque "invalid
// namespace" error partway through a large load/run.
var mongoDatabaseNameRe = regexp.MustCompile(`^[^/\\. "$*<>:|?]{1,64}$`)

type mongoDB struct {
	cli *mongo.Client
	db  *mongo.Database
	// unacknowledgedWrites is true when mongodb.write_concern=0: the driver
	// never waits for (or receives) a server reply for a write, so
	// InsertOneResult/UpdateResult/DeleteResult are always left at their
	// zero values - MatchedCount/DeletedCount can no longer be used to
	// detect a missing key, and a real server-side write failure can no
	// longer be observed via err either. Both are inherent to w=0, not bugs
	// in this adapter, so Update/Delete treat "not found" as unknowable
	// instead of reporting it (wrongly) as a hard failure on every call.
	unacknowledgedWrites bool
}

func (m *mongoDB) Close() error {
	return m.cli.Disconnect(context.Background())
}

func (m *mongoDB) InitThread(ctx context.Context, threadID int, threadCount int) context.Context {
	return ctx
}

func (m *mongoDB) CleanupThread(ctx context.Context) {
}

// Read a document.
func (m *mongoDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	projection := map[string]bool{"_id": false}
	for _, field := range fields {
		projection[field] = true
	}
	opt := &options.FindOneOptions{Projection: projection}
	var doc map[string][]byte
	if err := m.db.Collection(table).FindOne(ctx, bson.M{"_id": key}, opt).Decode(&doc); err != nil {
		return nil, fmt.Errorf("Read error: %s", err.Error())
	}
	return doc, nil
}

// Scan documents.
func (m *mongoDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	projection := map[string]bool{"_id": false}
	for _, field := range fields {
		projection[field] = true
	}
	limit := int64(count)
	opt := &options.FindOptions{Projection: projection, Sort: bson.M{"_id": 1}, Limit: &limit}
	cursor, err := m.db.Collection(table).Find(ctx, bson.M{"_id": bson.M{"$gte": startKey}}, opt)
	if err != nil {
		return nil, fmt.Errorf("Scan error: %s", err.Error())
	}
	defer cursor.Close(ctx)

	var docs []map[string][]byte
	if err = cursor.All(ctx, &docs); err != nil {
		return nil, err
	}

	return docs, nil
}

// Insert a document.
func (m *mongoDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	doc := bson.M{"_id": key}
	for k, v := range values {
		doc[k] = v
	}
	if _, err := m.db.Collection(table).InsertOne(ctx, doc); err != nil && err != mongo.ErrUnacknowledgedWrite {
		fmt.Println(err)
		return fmt.Errorf("Insert error: %s", err.Error())
	}
	return nil
}

// Update a document.
func (m *mongoDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	res, err := m.db.Collection(table).UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$set": values})
	// With mongodb.write_concern=0 the driver deliberately returns
	// ErrUnacknowledgedWrite from every call - by design, an unacknowledged
	// write can't be confirmed - and res is left zero-valued, not a real
	// failure. m.unacknowledgedWrites still guards the MatchedCount check
	// below as a second line of defense in case a future driver version
	// stops erroring here and starts returning a zero-valued result instead.
	if err != nil && err != mongo.ErrUnacknowledgedWrite {
		return fmt.Errorf("Update error: %s", err.Error())
	}
	if !m.unacknowledgedWrites && res.MatchedCount != 1 {
		return fmt.Errorf("Update error: %s not found", key)
	}
	return nil
}

// Delete a document.
func (m *mongoDB) Delete(ctx context.Context, table string, key string) error {
	res, err := m.db.Collection(table).DeleteOne(ctx, bson.M{"_id": key})
	if err != nil && err != mongo.ErrUnacknowledgedWrite {
		return fmt.Errorf("Delete error: %s", err.Error())
	}
	if !m.unacknowledgedWrites && res.DeletedCount != 1 {
		return fmt.Errorf("Delete error: %s not found", key)
	}
	return nil
}

type mongodbCreator struct{}

func (c mongodbCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	uri := p.GetString(mongodbUrl, mongodbUrlDefault)
	authdb := p.GetString(mongodbAuthdb, mongodbAuthdbDefault)
	tlsSkipVerify := p.GetBool(mongodbTLSSkipVerify, false)
	caFile := p.GetString(mongodbTLSCAFile, "")

	connString, err := connstring.Parse(uri)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cliOpts := options.Client().ApplyURI(uri)
	// The driver only populates TLSConfig when the URI itself contains
	// tls=true/ssl=true; without this, setting mongodb.tls_ca_file or
	// mongodb.tls_skip_verify alone silently does nothing - the properties
	// are made self-sufficient here instead of depending on an undocumented
	// URL flag the operator has no reason to know about.
	if cliOpts.TLSConfig == nil && (tlsSkipVerify || caFile != "") {
		cliOpts.TLSConfig = &tls.Config{}
	}
	if cliOpts.TLSConfig != nil {
		if len(connString.Hosts) > 0 {
			servername := strings.Split(connString.Hosts[0], ":")[0]
			log.Printf("using server name for tls: %s\n", servername)
			cliOpts.TLSConfig.ServerName = servername
		}
		if tlsSkipVerify {
			log.Println("skipping tls cert validation")
			cliOpts.TLSConfig.InsecureSkipVerify = true
		}

		if caFile != "" {
			// Load CA cert. Returned as an error (not log.Fatal, which would
			// os.Exit(1) here and skip any cleanup the caller might do)
			// so a bad cert path fails like any other config error.
			caCert, err := ioutil.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read %s: %w", mongodbTLSCAFile, err)
			}
			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
				return nil, fmt.Errorf("%s %q: certificate could not be parsed", mongodbTLSCAFile, caFile)
			}

			cliOpts.TLSConfig.RootCAs = caCertPool
		}
	}
	t := uint64(p.GetInt64(prop.ThreadCount, prop.ThreadCountDefault))
	cliOpts.SetMaxPoolSize(t)

	if socketTimeout, ok := p.Get(mongodbSocketTimeout); ok {
		d, err := time.ParseDuration(socketTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", mongodbSocketTimeout, socketTimeout, err)
		}
		cliOpts.SetSocketTimeout(d)
	}

	wc, wcOk := p.Get(mongodbWriteConcern)
	journal := p.GetBool(mongodbWriteConcernJournal, false)
	unacknowledgedWrites := false
	if wcOk || journal {
		var wcOpts []writeconcern.Option
		if wcOk {
			if strings.EqualFold(wc, "majority") {
				wcOpts = append(wcOpts, writeconcern.WMajority())
			} else {
				w, err := strconv.Atoi(wc)
				if err != nil {
					return nil, fmt.Errorf("invalid %s %q: must be \"majority\" or an integer ack count", mongodbWriteConcern, wc)
				}
				if w == 0 {
					unacknowledgedWrites = true
				}
				wcOpts = append(wcOpts, writeconcern.W(w))
			}
		}
		if journal {
			if unacknowledgedWrites {
				return nil, fmt.Errorf("%s=true is incompatible with %s=0 (an unacknowledged write cannot also require a journal ack)", mongodbWriteConcernJournal, mongodbWriteConcern)
			}
			wcOpts = append(wcOpts, writeconcern.J(true))
		}
		if wcTimeout, ok := p.Get(mongodbWriteConcernTimeout); ok {
			if unacknowledgedWrites {
				return nil, fmt.Errorf("%s is incompatible with %s=0 (an unacknowledged write has no server-side wait to time out)", mongodbWriteConcernTimeout, mongodbWriteConcern)
			}
			d, err := time.ParseDuration(wcTimeout)
			if err != nil {
				return nil, fmt.Errorf("invalid %s %q: %w", mongodbWriteConcernTimeout, wcTimeout, err)
			}
			wcOpts = append(wcOpts, writeconcern.WTimeout(d))
		}
		cliOpts.SetWriteConcern(writeconcern.New(wcOpts...))
	}

	if rc, ok := p.Get(mongodbReadConcern); ok {
		// "snapshot" is deliberately not accepted: per the driver's own
		// readconcern.Snapshot() doc, it is only valid inside a
		// multi-document transaction/session, and this adapter's Read/Scan
		// never open one - every read would fail server-side instead of
		// failing this one config check up front.
		switch strings.ToLower(rc) {
		case "local", "available", "majority", "linearizable":
			cliOpts.SetReadConcern(readconcern.New(readconcern.Level(strings.ToLower(rc))))
		default:
			return nil, fmt.Errorf("unknown %s %q: expected local, available, majority, or linearizable", mongodbReadConcern, rc)
		}
	}

	if rp, ok := p.Get(mongodbReadPreference); ok {
		var pref *readpref.ReadPref
		switch strings.ToLower(rp) {
		case "primary":
			pref = readpref.Primary()
		case "primarypreferred":
			pref = readpref.PrimaryPreferred()
		case "secondary":
			pref = readpref.Secondary()
		case "secondarypreferred":
			pref = readpref.SecondaryPreferred()
		case "nearest":
			pref = readpref.Nearest()
		default:
			return nil, fmt.Errorf("unknown %s %q: expected primary, primaryPreferred, secondary, secondaryPreferred, or nearest", mongodbReadPreference, rp)
		}
		cliOpts.SetReadPreference(pref)
	}

	username, usrExist := p.Get(mongodbUsername)
	password, pwdExist := p.Get(mongodbPassword)
	if usrExist && pwdExist {
		cliOpts.SetAuth(options.Credential{AuthSource: authdb, Username: username, Password: password})
	} else if usrExist {
		return nil, errors.New("mongodb.username is set, but mongodb.password is missing")
	} else if pwdExist {
		return nil, errors.New("mongodb.password is set, but mongodb.username is missing")
	}

	dbName := mongodbDatabaseDefault
	if connString.Database != "" {
		dbName = connString.Database
	}
	if !mongoDatabaseNameRe.MatchString(dbName) {
		return nil, fmt.Errorf("invalid database name %q resolved from %s: must be 1-64 characters and may not contain / \\ . \" $ * < > : | ? or a space", dbName, mongodbUrl)
	}

	cli, err := mongo.Connect(ctx, cliOpts)
	if err != nil {
		return nil, err
	}
	// Ping performs (and thus validates) the full connection handshake,
	// including authentication - a separate ListDatabaseNames call used to
	// run here purely to "check auth", but that command needs a cluster-wide
	// privilege (e.g. clusterMonitor) a normal least-privilege readWrite
	// service account doesn't have, so it rejected valid credentials and
	// discarded the real driver error (topology/TLS/auth) behind a fixed
	// "auth failed" string. Removed: Ping already proves auth works, and any
	// permission gap the target user genuinely lacks will surface clearly
	// from the first real Read/Insert/Update/Delete instead.
	if err := cli.Ping(ctx, nil); err != nil {
		return nil, err
	}
	fmt.Printf("Connected to MongoDB! Using database %q\n", dbName)

	m := &mongoDB{
		cli:                  cli,
		db:                   cli.Database(dbName),
		unacknowledgedWrites: unacknowledgedWrites,
	}
	return m, nil
}

func init() {
	ycsb.RegisterDBCreator("mongodb", mongodbCreator{})
}
