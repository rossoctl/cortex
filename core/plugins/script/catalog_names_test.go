package script

import (
	"testing"

	"github.com/rossoctl/cortex/core/plugins/script/catalog"
)

// Every catalog entry's name is a dir this plugin accepts, so an installed entry's
// slot loads. The catalog checks names by its own copy of the rule; this holds the
// copy to SlotDir.
func TestCatalog_NamesAreSlotDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entries, err := catalog.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := SlotDir(e.Name); err != nil {
			t.Errorf("%s: %v", e.Name, err)
		}
	}
}
