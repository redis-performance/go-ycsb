// Copyright (c) 2020 Daimler TSS GmbH TLS support

package mongodb

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"io/ioutil"
	"log"
	"strconv"
	"strings"

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
	// mongodbReadConcern: "local", "available", "majority", "linearizable", or
	// "snapshot" - the consistency/durability guarantee of data returned by a
	// read (e.g. mongodb.read_concern=majority to only read majority-committed data).
	mongodbReadConcern = "mongodb.read_concern"
	// mongodbReadPreference: "primary", "primaryPreferred", "secondary",
	// "secondaryPreferred", or "nearest" - which replica-set member(s) reads
	// are routed to. This is a distinct knob from mongodbReadConcern: read
	// preference picks WHERE a read goes, read concern picks HOW committed
	// the data it returns must be.
	mongodbReadPreference = "mongodb.read_preference"
)

type mongoDB struct {
	cli *mongo.Client
	db  *mongo.Database
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
	if _, err := m.db.Collection(table).InsertOne(ctx, doc); err != nil {
		fmt.Println(err)
		return fmt.Errorf("Insert error: %s", err.Error())
	}
	return nil
}

// Update a document.
func (m *mongoDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	res, err := m.db.Collection(table).UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$set": values})
	if err != nil {
		return fmt.Errorf("Update error: %s", err.Error())
	}
	if res.MatchedCount != 1 {
		return fmt.Errorf("Update error: %s not found", key)
	}
	return nil
}

// Delete a document.
func (m *mongoDB) Delete(ctx context.Context, table string, key string) error {
	res, err := m.db.Collection(table).DeleteOne(ctx, bson.M{"_id": key})
	if err != nil {
		return fmt.Errorf("Delete error: %s", err.Error())
	}
	if res.DeletedCount != 1 {
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
			// Load CA cert
			caCert, err := ioutil.ReadFile(caFile)
			if err != nil {
				log.Fatal(err)
			}
			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
				log.Fatalf("certifacte %s could not be parsed", caFile)
			}

			cliOpts.TLSConfig.RootCAs = caCertPool
		}
	}
	t := uint64(p.GetInt64(prop.ThreadCount, prop.ThreadCountDefault))
	cliOpts.SetMaxPoolSize(t)

	if wc, ok := p.Get(mongodbWriteConcern); ok {
		var wcOpts []writeconcern.Option
		if strings.EqualFold(wc, "majority") {
			wcOpts = append(wcOpts, writeconcern.WMajority())
		} else {
			w, err := strconv.Atoi(wc)
			if err != nil {
				return nil, fmt.Errorf("invalid %s %q: must be \"majority\" or an integer ack count", mongodbWriteConcern, wc)
			}
			wcOpts = append(wcOpts, writeconcern.W(w))
		}
		if p.GetBool(mongodbWriteConcernJournal, false) {
			wcOpts = append(wcOpts, writeconcern.J(true))
		}
		cliOpts.SetWriteConcern(writeconcern.New(wcOpts...))
	}

	if rc, ok := p.Get(mongodbReadConcern); ok {
		cliOpts.SetReadConcern(readconcern.New(readconcern.Level(strings.ToLower(rc))))
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

	cli, err := mongo.Connect(ctx, cliOpts)
	if err != nil {
		return nil, err
	}
	if err := cli.Ping(ctx, nil); err != nil {
		return nil, err
	}
	// check if auth passed
	if _, err := cli.ListDatabaseNames(ctx, map[string]string{}); err != nil {
		return nil, errors.New("auth failed")
	}

	fmt.Println("Connected to MongoDB!")

	dbName := mongodbDatabaseDefault
	if connString.Database != "" {
		dbName = connString.Database
	}
	m := &mongoDB{
		cli: cli,
		db:  cli.Database(dbName),
	}
	return m, nil
}

func init() {
	ycsb.RegisterDBCreator("mongodb", mongodbCreator{})
}
