package sio

import (
	"strings"
	"testing"
	"time"
)

func TestPacketRoundTrip(t *testing.T) {
	cases := []struct {
		encoded string
		p       packet
	}{
		{"0", packet{Type: sioConnect, Namespace: "/", AckID: -1}},
		{`0/storage,{"token":"x"}`, packet{Type: sioConnect, Namespace: "/storage", AckID: -1, Data: []byte(`{"token":"x"}`)}},
		{`2["dataUpdate",{"a":1}]`, packet{Type: sioEvent, Namespace: "/", AckID: -1, Data: []byte(`["dataUpdate",{"a":1}]`)}},
		{`2/alarm,12["subscribe",{}]`, packet{Type: sioEvent, Namespace: "/alarm", AckID: 12, Data: []byte(`["subscribe",{}]`)}},
		{`3/storage,0[{"success":true}]`, packet{Type: sioAck, Namespace: "/storage", AckID: 0, Data: []byte(`[{"success":true}]`)}},
		{"1/storage,", packet{Type: sioDisconnect, Namespace: "/storage", AckID: -1}},
	}

	for _, c := range cases {
		got, err := decodePacket(c.encoded)
		if err != nil {
			t.Fatalf("decode %q: %v", c.encoded, err)
		}
		if got.Type != c.p.Type || got.Namespace != c.p.Namespace || got.AckID != c.p.AckID || string(got.Data) != string(c.p.Data) {
			t.Errorf("decode %q got %+v want %+v", c.encoded, got, c.p)
		}

		if enc := encodePacket(c.p); enc != c.encoded {
			t.Errorf("encode %+v got %q want %q", c.p, enc, c.encoded)
		}
	}
}

func TestSocketURL(t *testing.T) {
	cases := []struct{ in, ws, ns string }{
		{"http://localhost:1337/", "ws://localhost:1337/socket.io/?EIO=4&transport=websocket", "/"},
		{"http://localhost:1337", "ws://localhost:1337/socket.io/?EIO=4&transport=websocket", "/"},
		{"https://x.ns.localhost/storage", "wss://x.ns.localhost/socket.io/?EIO=4&transport=websocket", "/storage"},
		{"http://localhost:1337/alarm?token=abc", "ws://localhost:1337/socket.io/?EIO=4&token=abc&transport=websocket", "/alarm"},
	}
	for _, c := range cases {
		ws, ns, err := SocketURL(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if ws != c.ws || ns != c.ns {
			t.Errorf("SocketURL(%q) = %q, %q want %q, %q", c.in, ws, ns, c.ws, c.ns)
		}
	}
}

func TestParseScript(t *testing.T) {
	script := `
# comment
NAME conn
CONNECT http://{{ .site }}/storage
H X-Test: 1

NAME sub
EMIT subscribe
B {"accessToken": "{{ token }}",
  "collections": ["entries"]}
B 2
ACK
T 5s

NAME create
POST http://{{ .site }}/api/v3/entries
H Authorization: Bearer {{ jwt }}
B {"sgv": 100}

NAME created
WAIT create
C conn

NAME everything
COLLECT 1s

NAME bye
DISCONNECT
`
	steps, err := parseScript(strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 6 {
		t.Fatalf("got %d steps", len(steps))
	}

	if s := steps[0]; s.Verb != VerbConnect || s.Target != "http://{{ .site }}/storage" || s.Headers.Get("X-Test") != "1" {
		t.Errorf("bad connect step %+v", s)
	}
	if s := steps[1]; s.Verb != VerbEmit || !s.Ack || s.Timeout != 5*time.Second || len(s.Args) != 2 ||
		s.Args[0] != "{\"accessToken\": \"{{ token }}\",\n\"collections\": [\"entries\"]}" {
		t.Errorf("bad emit step %+v", s)
	}
	if s := steps[2]; s.HTTP == nil || s.HTTP.Method != "POST" || s.HTTP.Body != `{"sgv": 100}` {
		t.Errorf("bad http step %+v", s)
	}
	if s := steps[3]; s.Verb != VerbWait || s.Conn != "conn" || s.Timeout != DefaultTimeout {
		t.Errorf("bad wait step %+v", s)
	}

	if err := Validate([]*ScriptFile{{Name: "test", Steps: steps}}, nil); err != nil {
		t.Errorf("validate: %v", err)
	}
	if err := Validate([]*ScriptFile{{Name: "test", Steps: steps}}, map[string]bool{"sub": true}); err == nil {
		t.Errorf("expected duplicate name error")
	}
}

func TestParseScriptErrors(t *testing.T) {
	bad := []string{
		"CONNECT http://x/",
		"NAME a\nEMIT\n",
		"NAME a\nWAIT x\nB {}\n",
		"NAME a\nCOLLECT soon\n",
		"NAME a\nWAIT x\nACK\n",
		"NAME a\nEMIT x\nZ what\n",
	}
	for _, s := range bad {
		if _, err := parseScript(strings.NewReader(s)); err == nil {
			t.Errorf("expected error parsing %q", s)
		}
	}

	steps, _ := parseScript(strings.NewReader("NAME a\nWAIT x\n"))
	if err := Validate([]*ScriptFile{{Name: "test", Steps: steps}}, nil); err == nil {
		t.Errorf("expected WAIT before CONNECT to fail validation")
	}
}
