package main

import (
	"encoding/json"
	"testing"
)

func TestKVMapSet(t *testing.T) {
	var k kvMap
	for _, v := range []string{
		"site=localhost:1337",
		"jwt.token=abc",
		"jwt.read=def",
		"a.b.c=deep",
		"query=a=b=c",
		"empty=",
		"site=overwritten",
	} {
		if err := k.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}

	got, _ := json.Marshal(map[string]any(k))
	want := `{"a":{"b":{"c":"deep"}},"empty":"","jwt":{"read":"def","token":"abc"},"query":"a=b=c","site":"overwritten"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if k.String() == "" {
		t.Error("String should describe the map")
	}
}

func TestKVMapSetErrors(t *testing.T) {
	for _, v := range []string{"", "novalue", "_private=x", "_=x"} {
		var k kvMap
		if err := k.Set(v); err == nil {
			t.Errorf("Set(%q) should fail", v)
		}
	}

	// a key that is already a value cannot have children
	var k kvMap
	if err := k.Set("site=x"); err != nil {
		t.Fatal(err)
	}
	if err := k.Set("site.port=1"); err == nil {
		t.Error("expected error nesting under a value")
	}
}

func TestKVMapEmptyString(t *testing.T) {
	var k kvMap
	if k.String() != "map[]" {
		t.Errorf("got %q", k.String())
	}
}
