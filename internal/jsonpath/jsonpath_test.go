package jsonpath

import "testing"

func TestLookup(t *testing.T) {
	payload := []byte(`{"type":"charge.succeeded","data":{"object":{"status":"succeeded","amount":2500,"paid":true,"note":null}},"items":[{"id":"a"},{"id":"b"}]}`)
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"type", "charge.succeeded", true},
		{"$.type", "charge.succeeded", true},
		{"$.data.object.status", "succeeded", true},
		{"data.object.amount", "2500", true},
		{"data.object.paid", "true", true},
		{"data.object.note", "null", true},
		{"items[0].id", "a", true},
		{"items[1].id", "b", true},
		{"items[2].id", "", false},
		{"items[0].missing", "", false},
		{"data.missing", "", false},
		{"data.object.status.x", "", false}, // string is not an object
		{"", "", false},
		{"type[0]", "", false}, // string is not an array
		{"data.object.amount[-1]", "", false},
		{"data.object.amount[abc]", "", false},
	}
	for _, c := range cases {
		v, ok := Lookup(payload, c.path)
		if ok != c.ok {
			t.Fatalf("Lookup(%q): ok=%v, want %v", c.path, ok, c.ok)
		}
		if ok && stringify(v) != c.want {
			t.Fatalf("Lookup(%q): got %q, want %q", c.path, stringify(v), c.want)
		}
	}
	// Non-JSON payload never resolves.
	if _, ok := Lookup([]byte(`not json`), "type"); ok {
		t.Fatal("non-JSON payload should not resolve")
	}
}

func TestMatch(t *testing.T) {
	payload := []byte(`{"type":"payment_intent.succeeded","data":{"object":{"status":"succeeded","amount":2500}}}`)
	if !Match(payload, "type=payment_intent.succeeded") {
		t.Fatal("exact string match should pass")
	}
	if !Match(payload, "data.object.status=succeeded") {
		t.Fatal("nested match should pass")
	}
	if !Match(payload, "data.object.amount=2500") {
		t.Fatal("number match should pass")
	}
	if Match(payload, "type=charge.failed") {
		t.Fatal("wrong value should not match")
	}
	if Match(payload, "data.missing=x") {
		t.Fatal("missing path should not match")
	}
	if Match(payload, "type") {
		t.Fatal("expression without '=' should not match")
	}
	if Match(payload, "=x") {
		t.Fatal("empty path should not match")
	}
	if Match([]byte(`not json`), "type=x") {
		t.Fatal("non-JSON payload should not match")
	}
	if Match(payload, " type = payment_intent.succeeded ") {
		// Spaces around path and value are trimmed.
	} else {
		t.Fatal("spaces around path/value should be trimmed")
	}
}

func TestRenderTemplate(t *testing.T) {
	payload := []byte(`{"type":"charge.succeeded","data":{"object":{"id":"ch_1","amount":2500}}}`)
	got := RenderTemplate(payload, "type={{.type}} id={{.data.object.id}} amount={{.data.object.amount}}")
	want := "type=charge.succeeded id=ch_1 amount=2500"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Missing path renders empty (the "=" is literal template text).
	if got := RenderTemplate(payload, "x={{.missing}}y"); got != "x=y" {
		t.Fatalf("missing path: got %q, want %q", got, "x=y")
	}
	// No placeholders: unchanged.
	if got := RenderTemplate(payload, "static-value"); got != "static-value" {
		t.Fatalf("static template: got %q", got)
	}
	// Unclosed placeholder left as-is.
	if got := RenderTemplate(payload, "x={{.type"); got != "x={{.type" {
		t.Fatalf("unclosed placeholder: got %q", got)
	}
}
