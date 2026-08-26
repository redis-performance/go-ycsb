package cosmosdb

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/magiconair/properties"
)

func TestJSONPointerEscaper(t *testing.T) {
	tests := []struct {
		field string
		want  string
	}{
		{"field0", "field0"},
		{"event_ts", "event_ts"},
		{"a/b", "a~1b"},
		{"a~b", "a~0b"},
		{"a/b~c", "a~1b~0c"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if got := jsonPointerEscaper.Replace(tt.field); got != tt.want {
				t.Fatalf("jsonPointerEscaper.Replace(%q) = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

func TestCanUsePatchUpdate(t *testing.T) {
	fields := func(n int) map[string][]byte {
		values := make(map[string][]byte, n)
		for i := 0; i < n; i++ {
			values[fmt.Sprintf("field%d", i)] = []byte("v")
		}
		return values
	}

	tests := []struct {
		name   string
		values map[string][]byte
		want   bool
	}{
		{"empty", map[string][]byte{}, true},
		{"single field", map[string][]byte{"field0": []byte("v")}, true},
		{"exactly at the patch cap", fields(cosmosMaxPatchOps), true},
		{"one over the patch cap", fields(cosmosMaxPatchOps + 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canUsePatchUpdate(tt.values); got != tt.want {
				t.Fatalf("canUsePatchUpdate(%d fields) = %v, want %v", len(tt.values), got, tt.want)
			}
		})
	}
}

// TestDecodeDoc guards against a Cosmos DB system property (id, _rid,
// _self, _etag, _attachments, _ts - injected into every document Cosmos DB
// returns) leaking into a Read/Scan result as if it were one of the
// record's own fields.
func TestDecodeDoc(t *testing.T) {
	raw := []byte(`{"id":"user0","_rid":"abc","_self":"dbs/x","_etag":"\"00\"","_attachments":"attachments/","_ts":123,"field0":"aGVsbG8=","field1":"d29ybGQ="}`)
	doc, err := decodeDoc(raw)
	if err != nil {
		t.Fatalf("decodeDoc: %v", err)
	}
	if len(doc) != 2 {
		t.Fatalf("decodeDoc returned %d fields, want 2 (system fields must be stripped): %v", len(doc), doc)
	}
	if string(doc["field0"]) != "hello" || string(doc["field1"]) != "world" {
		t.Fatalf("decodeDoc did not base64-decode field values correctly: %v", doc)
	}
}

func TestFilterFields(t *testing.T) {
	doc := map[string][]byte{"field0": []byte("a"), "field1": []byte("b"), "field2": []byte("c")}

	if got := filterFields(doc, nil); len(got) != 3 {
		t.Fatalf("filterFields with no fields should return everything, got %d fields", len(got))
	}

	got := filterFields(doc, []string{"field1"})
	if len(got) != 1 || string(got["field1"]) != "b" {
		t.Fatalf("filterFields([field1]) = %v, want only field1=b", got)
	}
}

func TestParseConsistencyLevel(t *testing.T) {
	tests := []struct {
		name    string
		value   string // "" means the property is left unset
		want    *azcosmos.ConsistencyLevel
		wantErr bool
	}{
		{"unset", "", nil, false},
		{"Strong", "Strong", azcosmos.ConsistencyLevelStrong.ToPtr(), false},
		{"case insensitive", "strong", azcosmos.ConsistencyLevelStrong.ToPtr(), false},
		{"unknown", "Eventualish", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := properties.NewProperties()
			if tt.value != "" {
				if _, _, err := p.Set(cosmosConsistencyLevel, tt.value); err != nil {
					t.Fatalf("p.Set: %v", err)
				}
			}
			got, err := parseConsistencyLevel(p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseConsistencyLevel(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if tt.want == nil && got != nil {
				t.Fatalf("parseConsistencyLevel(%q) = %v, want nil", tt.value, *got)
			}
			if tt.want != nil && (got == nil || *got != *tt.want) {
				t.Fatalf("parseConsistencyLevel(%q) = %v, want %v", tt.value, got, *tt.want)
			}
		})
	}
}
