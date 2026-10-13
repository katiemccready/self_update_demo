package appstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type v1 struct {
	Count int `json:"count"`
}
type v2 struct {
	Total int    `json:"total"`
	Unit  string `json:"unit"`
}

var schema1 = Schema{Current: 1, Migrate: map[int]func(json.RawMessage) (json.RawMessage, error){
	0: func(d json.RawMessage) (json.RawMessage, error) {
		var o struct{ N int }
		if err := json.Unmarshal(d, &o); err != nil {
			return nil, err
		}
		return json.Marshal(v1{Count: o.N})
	},
}}

var schema2 = Schema{Current: 2, Migrate: map[int]func(json.RawMessage) (json.RawMessage, error){
	0: schema1.Migrate[0],
	1: func(d json.RawMessage) (json.RawMessage, error) {
		var o v1
		if err := json.Unmarshal(d, &o); err != nil {
			return nil, err
		}
		return json.Marshal(v2{Total: o.Count, Unit: "items"})
	},
}}

func tmp(t *testing.T) string { return filepath.Join(t.TempDir(), "s.json") }

func TestVersionedRoundTripAndMissing(t *testing.T) {
	p := tmp(t)
	var got v1
	if err := LoadVersioned(p, schema1, &got); err != nil || got.Count != 0 {
		t.Fatalf("missing: %v %+v", err, got)
	}
	if err := SaveVersioned(p, schema1, v1{Count: 4}); err != nil {
		t.Fatal(err)
	}
	if err := LoadVersioned(p, schema1, &got); err != nil || got.Count != 4 {
		t.Fatalf("round trip: %v %+v", err, got)
	}
}

func TestLegacyUnversionedFileIsMigratedWithBackup(t *testing.T) {
	p := tmp(t)
	legacy := `{"n":9}`
	os.WriteFile(p, []byte(legacy), 0o600)
	var got v1
	if err := LoadVersioned(p, schema1, &got); err != nil || got.Count != 9 {
		t.Fatalf("load: %v %+v", err, got)
	}
	if b, _ := os.ReadFile(p + ".v0.bak"); string(b) != legacy {
		t.Errorf("pre-migration backup = %q, want the original bytes", b)
	}
	var env struct {
		Schema int `json:"_schema"`
		Count  int `json:"count"`
	}
	b, _ := os.ReadFile(p)
	if json.Unmarshal(b, &env) != nil || env.Schema != 1 || env.Count != 9 {
		t.Errorf("file not rewritten as schema 1 with fields at top level: %s", b)
	}
}

func TestMigrationChainRunsEveryStepInOrder(t *testing.T) {
	p := tmp(t)
	os.WriteFile(p, []byte(`{"n":3}`), 0o600)
	var got v2
	if err := LoadVersioned(p, schema2, &got); err != nil || got != (v2{3, "items"}) {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestFailedMigrationLeavesFileUntouched(t *testing.T) {
	p := tmp(t)
	orig := `{"n":"not a number"}`
	os.WriteFile(p, []byte(orig), 0o600)
	var got v1
	err := LoadVersioned(p, schema1, &got)
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("err = %v, want ErrMigration", err)
	}
	if b, _ := os.ReadFile(p); string(b) != orig {
		t.Errorf("file changed by failed migration: %s", b)
	}
	if _, err := os.Stat(p + ".v0.bak"); err == nil {
		t.Error("backup written for a migration that never completed")
	}
}

func TestMissingMigrationStepIsAnErrorNotASkip(t *testing.T) {
	p := tmp(t)
	os.WriteFile(p, []byte(`{"n":1}`), 0o600)
	broken := Schema{Current: 1}
	if err := LoadVersioned(p, broken, &v1{}); !errors.Is(err, ErrMigration) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackupIsRefreshedOnEveryUpgrade(t *testing.T) {
	p := tmp(t)
	os.WriteFile(p, []byte(`{"n":5}`), 0o600)
	os.WriteFile(p+".v0.bak", []byte(`{"n":1}`), 0o600)
	if err := LoadVersioned(p, schema1, &v1{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p + ".v0.bak"); string(b) != `{"n":5}` {
		t.Errorf("stale backup kept: %s", b)
	}
}

func TestSecondRollbackRestoresLatestPreUpgradeState(t *testing.T) {
	p := tmp(t)
	type o struct {
		N int `json:"n"`
	}
	id := func(d json.RawMessage) (json.RawMessage, error) { return d, nil }
	s1 := Schema{Current: 1, Migrate: map[int]func(json.RawMessage) (json.RawMessage, error){0: id}}
	s2 := Schema{Current: 2, Migrate: map[int]func(json.RawMessage) (json.RawMessage, error){0: id, 1: id}}
	var v o
	SaveVersioned(p, s1, o{1})
	LoadVersioned(p, s2, &v)
	SaveVersioned(p, s2, o{50})
	if _, err := RestoreFromBackup(p, s1); err != nil {
		t.Fatal(err)
	}
	LoadVersioned(p, s1, &v)
	SaveVersioned(p, s1, o{900})
	LoadVersioned(p, s2, &v)
	SaveVersioned(p, s2, o{1000})
	if _, err := RestoreFromBackup(p, s1); err != nil {
		t.Fatal(err)
	}
	v = o{}
	if err := LoadVersioned(p, s1, &v); err != nil || v.N != 900 {
		t.Fatalf("second rollback restored n=%d (%v), want 900", v.N, err)
	}
}

func TestPreVersioningBuildStillReadsVersionedFile(t *testing.T) {
	p := tmp(t)
	type same struct {
		N int `json:"n"`
	}
	id := func(d json.RawMessage) (json.RawMessage, error) { return d, nil }
	additive := Schema{Current: 1, Migrate: map[int]func(json.RawMessage) (json.RawMessage, error){0: id}}
	if err := SaveVersioned(p, additive, same{N: 77}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	var legacy struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil || legacy.N != 77 {
		t.Fatalf("pre-versioning build reads n=%d (%v) from %s", legacy.N, err, raw)
	}
}

func TestEarlierNestedFormIsStillRead(t *testing.T) {
	p := tmp(t)
	os.WriteFile(p, []byte(`{"schema":1,"data":{"count":4}}`), 0o600)
	var got v1
	if err := LoadVersioned(p, schema1, &got); err != nil || got.Count != 4 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestSaveRejectsNonObjectAndReservedKey(t *testing.T) {
	p := tmp(t)
	if err := SaveVersioned(p, schema1, []int{1}); err == nil {
		t.Error("array state accepted")
	}
	if err := SaveVersioned(p, schema1, map[string]int{"_schema": 9}); err == nil {
		t.Error("reserved key accepted")
	}
}

func TestNewerSchemaIsRefusedAndUntouched(t *testing.T) {
	p := tmp(t)
	if err := SaveVersioned(p, schema2, v2{Total: 1, Unit: "x"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	err := LoadVersioned(p, schema1, &v1{})
	var ne *NewerSchemaError
	if !errors.As(err, &ne) || ne.Found != 2 || ne.Supported != 1 {
		t.Fatalf("err = %v, want NewerSchemaError{2,1}", err)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Error("newer state modified by an older build")
	}
}

func TestRollbackRestoresBackupAndKeepsNewerFile(t *testing.T) {
	p := tmp(t)
	os.WriteFile(p, []byte(`{"n":7}`), 0o600)

	if err := LoadVersioned(p, schema1, &v1{}); err != nil {
		t.Fatal(err)
	}
	if err := SaveVersioned(p, schema1, v1{Count: 8}); err != nil {
		t.Fatal(err)
	}

	old := Schema{Current: 0}
	var o struct{ N int }
	var ne *NewerSchemaError
	if err := LoadVersioned(p, old, &o); !errors.As(err, &ne) {
		t.Fatalf("err = %v", err)
	}
	from, err := RestoreFromBackup(p, old)
	if err != nil || from != 0 {
		t.Fatalf("restore: %d %v", from, err)
	}
	if err := LoadVersioned(p, old, &o); err != nil || o.N != 7 {
		t.Fatalf("after restore: %v %+v", err, o)
	}
	if _, err := os.Stat(p + ".v1.newer"); err != nil {
		t.Error("newer state not preserved:", err)
	}

	if err := LoadVersioned(p, schema1, &v1{}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreWithoutBackupFailsAndChangesNothing(t *testing.T) {
	p := tmp(t)
	SaveVersioned(p, schema2, v2{})
	before, _ := os.ReadFile(p)
	if _, err := RestoreFromBackup(p, schema1); err == nil {
		t.Fatal("expected error")
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Error("state modified")
	}
	if _, err := os.Stat(p + ".v2.newer"); err == nil {
		t.Error("state moved aside although nothing was restored")
	}
}

func TestRestoreSkipsUnreadableBackupForOlderOne(t *testing.T) {
	p := tmp(t)
	SaveVersioned(p, schema2, v2{})
	os.WriteFile(p+".v1.bak", []byte("garbage"), 0o600)
	os.WriteFile(p+".v0.bak", []byte(`{"n":2}`), 0o600)
	from, err := RestoreFromBackup(p, schema1)
	if err != nil || from != 0 {
		t.Fatalf("%d %v", from, err)
	}
}

func TestVersionedCorruptAndBadEnvelopes(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":       "not json",
		"trailing":      `{"schema":1,"data":{}} x`,
		"no data":       `{"schema":1}`,
		"bad schema":    `{"schema":"one","data":{}}`,
		"negative":      `{"schema":-1,"data":{}}`,
		"not an object": `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := tmp(t)
			os.WriteFile(p, []byte(content), 0o600)
			err := LoadVersioned(p, schema1, &v1{})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			if _, err := os.Stat(p + ".corrupt"); err != nil {
				t.Error("not set aside:", err)
			}
		})
	}
}

func TestLegacyFileWithSchemaFieldIsNotMistakenForNestedEnvelope(t *testing.T) {
	p := tmp(t)
	os.WriteFile(p, []byte(`{"schema":"orders","n":5}`), 0o600)
	type legacy struct {
		Schema string `json:"schema"`
		N      int    `json:"n"`
	}
	s := Schema{Current: 0}
	var got legacy
	if err := LoadVersioned(p, s, &got); err != nil || got.N != 5 || got.Schema != "orders" {
		t.Fatalf("err=%v got=%+v", err, got)
	}
	if _, err := os.Stat(p + ".corrupt"); err == nil {
		t.Error("a valid legacy file was quarantined")
	}
}

func TestTrailingDelimiterAfterDocumentIsCorrupt(t *testing.T) {
	for _, tail := range []string{"}", "]", "}}", " \n]"} {
		p := tmp(t)
		if err := os.WriteFile(p, []byte(`{"_schema":1,"count":3}`+tail), 0o600); err != nil {
			t.Fatal(err)
		}
		var got v1
		err := LoadVersioned(p, schema1, &got)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("tail %q: err = %v, want ErrCorrupt", tail, err)
		}
		if _, serr := os.Stat(p + ".corrupt"); serr != nil {
			t.Errorf("tail %q: damaged file was not set aside: %v", tail, serr)
		}
	}
}
