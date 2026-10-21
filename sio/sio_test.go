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

func TestParseScriptBoundaries(t *testing.T) {
	bad := map[string]string{
		"empty name":              "NAME \nCONNECT http://x/\n",
		"name only":               "NAME a\n",
		"connect without url":     "NAME a\nCONNECT\n",
		"connect two bodies":      "NAME a\nCONNECT http://x/\nB {}\nB {}\n",
		"connect with C":          "NAME a\nCONNECT http://x/\nC other\n",
		"header on emit":          "NAME a\nEMIT x\nH X: y\n",
		"header without colon":    "NAME a\nCONNECT http://x/\nH nocolon\n",
		"body on disconnect":      "NAME a\nDISCONNECT\nB {}\n",
		"ack on connect":          "NAME a\nCONNECT http://x/\nACK\n",
		"wait without event":      "NAME a\nWAIT \n",
		"collect without time":    "NAME a\nCOLLECT\n",
		"collect negative junk":   "NAME a\nCOLLECT -\n",
		"bad timeout":             "NAME a\nWAIT x\nT forever\n",
		"timeout without unit":    "NAME a\nWAIT x\nT 10\n",
		"lowercase verb is http":  "NAME a\nwait x\n",
		"http unknown line":       "NAME a\nGET http://x/\nWAIT y\n",
		"http header no colon":    "NAME a\nGET http://x/\nH broken\n",
		"line before first NAME":  "B {}\nNAME a\nWAIT x\n",
		"ack with trailing words": "NAME a\nEMIT x\nACK please\n",
	}
	for name, s := range bad {
		if _, err := parseScript(strings.NewReader(s)); err == nil {
			t.Errorf("%s: expected error parsing %q", name, s)
		}
	}

	good := map[string]struct {
		script string
		check  func(t *testing.T, steps []Step)
	}{
		"empty file": {"", func(t *testing.T, steps []Step) {
			if len(steps) != 0 {
				t.Errorf("got %d steps", len(steps))
			}
		}},
		"only comments": {"# a\n// b\n\n   \n", func(t *testing.T, steps []Step) {
			if len(steps) != 0 {
				t.Errorf("got %d steps", len(steps))
			}
		}},
		"comments and blank lines inside a step": {"NAME a\n# note\nEMIT x\n\n// another\nB 1\nACK\n", func(t *testing.T, steps []Step) {
			if len(steps) != 1 || steps[0].Verb != VerbEmit || len(steps[0].Args) != 1 || !steps[0].Ack {
				t.Errorf("got %+v", steps)
			}
		}},
		"indented NAME and verb": {"  NAME a\n  WAIT x\n", func(t *testing.T, steps []Step) {
			if len(steps) != 1 || steps[0].Target != "x" {
				t.Errorf("got %+v", steps)
			}
		}},
		"name with spaces": {"NAME my step\nDISCONNECT\n", func(t *testing.T, steps []Step) {
			if steps[0].Name != "my step" {
				t.Errorf("got %q", steps[0].Name)
			}
		}},
		"event with spaces": {"NAME a\nWAIT my event\n", func(t *testing.T, steps []Step) {
			if steps[0].Target != "my event" {
				t.Errorf("got %q", steps[0].Target)
			}
		}},
		"zero timeout": {"NAME a\nWAIT x\nT 0s\n", func(t *testing.T, steps []Step) {
			if steps[0].Timeout != 0 {
				t.Errorf("got %v", steps[0].Timeout)
			}
		}},
		"last timeout wins": {"NAME a\nWAIT x\nT 1s\nT 2s\n", func(t *testing.T, steps []Step) {
			if steps[0].Timeout != 2*time.Second {
				t.Errorf("got %v", steps[0].Timeout)
			}
		}},
		"header value with colons": {"NAME a\nCONNECT http://x/\nH X-Time: 10:20:30\n", func(t *testing.T, steps []Step) {
			if steps[0].Headers.Get("X-Time") != "10:20:30" {
				t.Errorf("got %q", steps[0].Headers.Get("X-Time"))
			}
		}},
		"repeated headers": {"NAME a\nCONNECT http://x/\nH X: 1\nH X: 2\n", func(t *testing.T, steps []Step) {
			if v := steps[0].Headers.Values("X"); len(v) != 2 {
				t.Errorf("got %v", v)
			}
		}},
		"tab continuation of body": {"NAME a\nEMIT x\nB [1,\n\t2]\n", func(t *testing.T, steps []Step) {
			if steps[0].Args[0] != "[1,\n2]" {
				t.Errorf("got %q", steps[0].Args[0])
			}
		}},
		"no trailing newline": {"NAME a\nWAIT x", func(t *testing.T, steps []Step) {
			if len(steps) != 1 {
				t.Errorf("got %d", len(steps))
			}
		}},
		"windows line endings": {"NAME a\r\nWAIT x\r\nT 1s\r\n", func(t *testing.T, steps []Step) {
			if steps[0].Target != "x" || steps[0].Timeout != time.Second {
				t.Errorf("got %+v", steps[0])
			}
		}},
		"http SET step": {"NAME a\nSET {{ .x }}\n", func(t *testing.T, steps []Step) {
			if steps[0].HTTP == nil || !steps[0].HTTP.IsMethod || steps[0].Verb != "SET" {
				t.Errorf("got %+v", steps[0])
			}
		}},
		"long body line": {"NAME a\nEMIT x\nB \"" + strings.Repeat("y", 200000) + "\"\n", func(t *testing.T, steps []Step) {
			if len(steps[0].Args[0]) != 200002 {
				t.Errorf("got %d", len(steps[0].Args[0]))
			}
		}},
	}
	for name, c := range good {
		steps, err := parseScript(strings.NewReader(c.script))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		t.Run(name, func(t *testing.T) { c.check(t, steps) })
	}
}

func TestValidateBoundaries(t *testing.T) {
	parse := func(name, s string) *ScriptFile {
		steps, err := parseScript(strings.NewReader(s))
		if err != nil {
			t.Fatal(err)
		}
		return &ScriptFile{Name: name, Steps: steps}
	}

	bad := map[string][]*ScriptFile{
		"duplicate inside a file":     {parse("a", "NAME x\nCONNECT http://h/\nNAME x\nWAIT y\n")},
		"duplicate across files":      {parse("a", "NAME x\nCONNECT http://h/\n"), parse("b", "NAME x\nCONNECT http://h/\n")},
		"C to a later CONNECT":        {parse("a", "NAME c1\nCONNECT http://h/\nNAME w\nWAIT y\nC c2\nNAME c2\nCONNECT http://h/\n")},
		"C to another file's CONNECT": {parse("a", "NAME c1\nCONNECT http://h/\n"), parse("b", "NAME c2\nCONNECT http://h/\nNAME w\nWAIT y\nC c1\n")},
		"C to a non CONNECT step":     {parse("a", "NAME c1\nCONNECT http://h/\nNAME w\nWAIT y\nNAME w2\nWAIT z\nC w\n")},
		"socket step before CONNECT":  {parse("a", "NAME d\nDISCONNECT\nNAME c\nCONNECT http://h/\n")},
		"http step without URL":       {parse("a", "NAME h\nH X: y\n")},
	}
	for name, scripts := range bad {
		if err := Validate(scripts, nil); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}

	ok := map[string][]*ScriptFile{
		"no scripts":             nil,
		"empty script":           {parse("a", "")},
		"http only":              {parse("a", "NAME h\nGET http://h/\n")},
		"CONNECT reused by name": {parse("a", "NAME c\nCONNECT http://h/\nNAME d\nDISCONNECT\nC c\nNAME w\nWAIT x\nC c\n")},
		"each file connects":     {parse("a", "NAME c1\nCONNECT http://h/\nNAME w1\nWAIT x\n"), parse("b", "NAME c2\nCONNECT http://h/\nNAME w2\nWAIT x\n")},
	}
	for name, scripts := range ok {
		if err := Validate(scripts, nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	if err := Validate([]*ScriptFile{parse("a", "NAME token\nCONNECT http://h/\n")}, map[string]bool{"token": true}); err == nil {
		t.Error("expected clash with an existing context name")
	}
}

func TestDecodePacketBoundaries(t *testing.T) {
	bad := []string{
		"",
		"7",
		"x",
		"/",
		`2["unterminated`,
		`5["bin"]`,
		`2{bad json}`,
		"2" + strings.Repeat("9", 40) + `["overflow"]`,
	}
	for _, s := range bad {
		if _, err := decodePacket(s); err == nil {
			t.Errorf("expected error decoding %q", s)
		}
	}

	good := map[string]packet{
		"0/storage":                     {Type: sioConnect, Namespace: "/storage", AckID: -1},
		"2/a/b,[\"x\"]":                 {Type: sioEvent, Namespace: "/a/b", AckID: -1, Data: []byte(`["x"]`)},
		"3/storage,0[]":                 {Type: sioAck, Namespace: "/storage", AckID: 0, Data: []byte(`[]`)},
		"2123":                          {Type: sioEvent, Namespace: "/", AckID: 123},
		`51-["bin",{"_placeholder":1}]`: {Type: sioBinaryEvent, Namespace: "/", AckID: -1, Data: []byte(`["bin",{"_placeholder":1}]`)},
		`4{"message":"nope"}`:           {Type: sioConnectError, Namespace: "/", AckID: -1, Data: []byte(`{"message":"nope"}`)},
		`2 ["leading space is json"]`:   {Type: sioEvent, Namespace: "/", AckID: -1, Data: []byte(` ["leading space is json"]`)},
	}
	for s, want := range good {
		got, err := decodePacket(s)
		if err != nil {
			t.Errorf("decode %q: %v", s, err)
			continue
		}
		if got.Type != want.Type || got.Namespace != want.Namespace || got.AckID != want.AckID || string(got.Data) != string(want.Data) {
			t.Errorf("decode %q got %+v want %+v", s, got, want)
		}
	}
}

func TestSocketURLBoundaries(t *testing.T) {
	for _, u := range []string{"", "localhost:1337", "ftp://h/", "mailto:x@y", "http://[::1"} {
		if _, _, err := SocketURL(u); err == nil {
			t.Errorf("expected error for %q", u)
		}
	}

	cases := []struct{ in, ws, ns string }{
		{"ws://h/", "ws://h/socket.io/?EIO=4&transport=websocket", "/"},
		{"wss://h/alarm", "wss://h/socket.io/?EIO=4&transport=websocket", "/alarm"},
		{"http://h/a/b/", "ws://h/socket.io/?EIO=4&transport=websocket", "/a/b"},
		{"http://user:pw@h:99/", "ws://user:pw@h:99/socket.io/?EIO=4&transport=websocket", "/"},
		// EIO and transport are always ours
		{"http://h/?EIO=3&transport=polling", "ws://h/socket.io/?EIO=4&transport=websocket", "/"},
		{"http://[::1]:1337/", "ws://[::1]:1337/socket.io/?EIO=4&transport=websocket", "/"},
	}
	for _, c := range cases {
		ws, ns, err := SocketURL(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if ws != c.ws || ns != c.ns {
			t.Errorf("SocketURL(%q) = %q, %q want %q, %q", c.in, ws, ns, c.ws, c.ns)
		}
	}
}
