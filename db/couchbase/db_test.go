package couchbase

import (
	"testing"

	"github.com/magiconair/properties"
)

func TestParseDurability(t *testing.T) {
	tests := []struct {
		name    string
		value   string // "" means the property is left unset
		wantErr bool
	}{
		{"unset defaults to none", "", false},
		{"none", "none", false},
		{"majority", "majority", false},
		{"majorityAndPersistActive", "majorityAndPersistActive", false},
		{"persistToMajority", "persistToMajority", false},
		{"case insensitive", "MAJORITY", false},
		{"unknown value", "bogus", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := properties.NewProperties()
			if tt.value != "" {
				if _, _, err := p.Set(couchbaseDurability, tt.value); err != nil {
					t.Fatalf("p.Set: %v", err)
				}
			}
			_, err := parseDurability(p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDurability(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

// TestValidateTLSScheme guards the fix for a real bug found by adversarial
// review: couchbase.tls_ca_file/tls_skip_verify used to be silently ignored
// whenever couchbase.connection_string wasn't couchbases://, connecting in
// plaintext with no error. This must keep failing loudly.
func TestValidateTLSScheme(t *testing.T) {
	tests := []struct {
		name          string
		connStr       string
		tlsSkipVerify bool
		caFile        string
		wantErr       bool
	}{
		{"plain scheme, no TLS options: fine", "couchbase://127.0.0.1", false, "", false},
		{"couchbases scheme, no TLS options: fine", "couchbases://cb.example.cloud.couchbase.com", false, "", false},
		{"couchbases scheme with skip_verify: fine", "couchbases://cb.example.cloud.couchbase.com", true, "", false},
		{"couchbases scheme with ca file: fine", "couchbases://cb.example.cloud.couchbase.com", false, "/etc/ca.pem", false},
		{"plain scheme with skip_verify: rejected", "couchbase://127.0.0.1", true, "", true},
		{"plain scheme with ca file: rejected", "couchbase://127.0.0.1", false, "/etc/ca.pem", true},
		{"plain scheme with both: rejected", "couchbase://127.0.0.1", true, "/etc/ca.pem", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTLSScheme(tt.connStr, tt.tlsSkipVerify, tt.caFile)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTLSScheme(%q, %v, %q) error = %v, wantErr %v", tt.connStr, tt.tlsSkipVerify, tt.caFile, err, tt.wantErr)
			}
		})
	}
}

// TestFieldPathSafe guards the fix for a real bug found by adversarial
// review: Update()'s MutateIn fast path used a field name directly as a
// Couchbase subdocument path, where '.'/'['/']' address nested/array
// locations instead of being literal characters - silently breaking updates
// for any field name containing them (e.g. a fieldnameprefix or
// lastfieldname value with a dot), while Insert/Read treat the identical
// name as an opaque literal key. Anything this regexp doesn't match must
// fall back to the always-correct Get+merge+Replace path instead.
func TestFieldPathSafe(t *testing.T) {
	tests := []struct {
		field string
		safe  bool
	}{
		{"field0", true},
		{"field49", true},
		{"event_ts", true},
		{"FieldName", true},
		{"a.0", false},
		{"event.ts", false},
		{"field[0]", false},
		{"a.b.c", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if got := fieldPathSafe.MatchString(tt.field); got != tt.safe {
				t.Fatalf("fieldPathSafe.MatchString(%q) = %v, want %v", tt.field, got, tt.safe)
			}
		})
	}
}
