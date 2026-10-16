package migrations

import "testing"

func TestMigrationsAreOrderedAndUnique(t *testing.T) {
	sorted := Migrations.Sorted()
	if len(sorted) == 0 {
		t.Fatal("no migrations registered")
	}
	seen := map[string]bool{}
	for i, m := range sorted {
		if seen[m.Name] {
			t.Errorf("duplicate migration name %q", m.Name)
		}
		seen[m.Name] = true
		if i > 0 && sorted[i-1].Name >= m.Name {
			t.Errorf("migrations out of order: %q then %q", sorted[i-1].Name, m.Name)
		}
		if m.Up == nil {
			t.Errorf("migration %q has no Up step", m.Name)
		}
	}
}
