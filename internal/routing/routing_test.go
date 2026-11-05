package routing

import (
	"testing"

	"github.com/mrpythonist/relaydeck/internal/database"
)

func TestMatch(t *testing.T) {
	d := &database.Destination{Enabled: true, SourceFilter: "stripe"}
	if !Match(d, "stripe", []byte(`{}`)) || Match(d, "github", []byte(`{}`)) {
		t.Fatal("source filter wrong")
	}
	d.SourceFilter = ""
	d.Filter = "paid"
	if !Match(d, "stripe", []byte(`{"type":"paid"}`)) || Match(d, "stripe", []byte(`{"type":"x"}`)) {
		t.Fatal("substring filter wrong")
	}
	d.Enabled = false
	if Match(d, "stripe", []byte(`{"type":"paid"}`)) {
		t.Fatal("disabled must not match")
	}
}
