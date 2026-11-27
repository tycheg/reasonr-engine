package conformance

import (
	"context"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
)

// RunEntitySink checks kind.EntitySink — the entity database of an engine
// kind that keeps its entities in memory: a row is written only if its
// version is greater than the stored one; personal fields read as written.
func RunEntitySink(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	sink, ok := f.st.Unit.(kind.EntitySink)
	if !ok {
		t.Skip("the store does not write entities (port.EntitySink)")
	}
	ctx := context.Background()
	keeper := f.typ("Keeper")
	id := f.id("Keeper")
	ti := f.s.Type(keeper)
	state := func(name, code string) reasonr.Record {
		fields := make([]reasonr.Value, len(ti.Fields))
		for i, fi := range ti.Fields {
			switch fi.Name {
			case "name":
				fields[i] = f.a.String(name)
			case "code":
				fields[i] = f.a.String(code)
			}
		}
		return f.a.Record(keeper, fields...)
	}
	check := func(version uint64, name, code string) {
		t.Helper()
		st, v, ok := f.load(id, "Keeper")
		if !ok || v != version {
			t.Fatalf("version %d (exists: %v), want %d", v, ok, version)
		}
		if got := f.a.Text(f.field(st, "Keeper", "name")); got != name {
			t.Fatalf("name = %q, want %q", got, name)
		}
		if got := f.a.Text(f.field(st, "Keeper", "code")); got != code {
			t.Fatalf("code = %q, want %q", got, code)
		}
	}
	put := func(version uint64, name, code string) {
		t.Helper()
		if err := sink.PutEntities(ctx, T1, f.a, []kind.EntityState{{Type: keeper, ID: id, State: state(name, code), Version: version}}); err != nil {
			t.Fatal(err)
		}
	}
	put(2, "Anna", "c1")
	check(2, "Anna", "c1")
	put(1, "Boris", "c2") // a late batch of an earlier writer
	check(2, "Anna", "c1")
	put(2, "Anna", "c1") // a repeated batch
	check(2, "Anna", "c1")
	put(5, "Vera", "c3")
	check(5, "Vera", "c3")
}
