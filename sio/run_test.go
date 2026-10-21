package sio

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runScript parses and executes script against the fake server, with the
// server's host as {{ .site }}
func runScript(t *testing.T, fs *fakeServer, script string, ctx map[string]any) *ScriptFile {
	t.Helper()
	steps, err := parseScript(strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	sf := &ScriptFile{Name: "test_script", Steps: steps}
	if err := Validate([]*ScriptFile{sf}, nil); err != nil {
		t.Fatal(err)
	}
	if ctx == nil {
		ctx = map[string]any{}
	}
	if fs != nil {
		ctx["site"] = fs.host()
	}
	sf.CTX = ctx
	if err := sf.Execute(&http.Client{}); err != nil {
		t.Fatal(err)
	}
	return sf
}

func TestExecuteScript(t *testing.T) {
	fs := newFakeServer(t)
	fs.onEvent = func(c *fakeConn, p packet) {
		name, args := eventName(p)
		switch name {
		case "subscribe":
			c.ack(p.Namespace, p.AckID, map[string]any{"success": true, "got": args})
		case "trigger":
			c.emit(p.Namespace, "create", map[string]any{"colName": "entries"})
			c.emit(p.Namespace, "create", "second", 2)
		}
	}

	sf := runScript(t, fs, `
NAME conn
CONNECT http://{{ .site }}/storage

NAME sub
EMIT subscribe
B {"accessToken": "{{ .token }}"}
B ["entries"]
ACK

NAME trigger
EMIT trigger

NAME created
WAIT create

NAME created_again
WAIT create

NAME nothing
WAIT never
T 50ms

NAME all
COLLECT 10ms

NAME bye
DISCONNECT

NAME after_bye
WAIT anything
`, map[string]any{"token": "tok"})

	want := []struct {
		name   string
		status int
		body   string
	}{
		{"conn", 200, `{"sid":"socket-sid"}`},
		{"sub", 200, `{"got":[{"accessToken":"tok"},["entries"]],"success":true}`},
		{"trigger", 200, `null`},
		{"created", 200, `{"colName":"entries"}`},
		{"created_again", 200, `["second",2]`},
		{"nothing", 408, `null`},
		{"all", 200, `[]`},
		{"bye", 200, `null`},
		{"after_bye", 500, `null`},
	}
	if len(sf.Results) != len(want) {
		t.Fatalf("got %d results", len(sf.Results))
	}
	for i, w := range want {
		r := sf.Results[i]
		if r.Step.Name != w.name || r.StatusCode != w.status || jsonString(r.Body) != w.body {
			t.Errorf("%d: got %s %d %s (%s) want %s %d %s", i, r.Step.Name, r.StatusCode, jsonString(r.Body), r.Error, w.name, w.status, w.body)
		}
		if (r.StatusCode == 200) != (r.Error == "") {
			t.Errorf("%s: status %d with error %q", r.Step.Name, r.StatusCode, r.Error)
		}
		// every result is in the context for later steps
		if v, ok := sf.CTX[w.name]; !ok || jsonString(v) != w.body {
			t.Errorf("ctx[%s] = %v", w.name, v)
		}
	}
	if !strings.Contains(sf.Results[8].Error, "not open") {
		t.Errorf("step after DISCONNECT should fail with not open, got %q", sf.Results[8].Error)
	}
}

func TestExecuteMultipleConnections(t *testing.T) {
	fs := newFakeServer(t)
	fs.onEvent = func(c *fakeConn, p packet) {
		name, _ := eventName(p)
		c.ack(p.Namespace, p.AckID, name+" on "+p.Namespace)
	}

	sf := runScript(t, fs, `
NAME root
CONNECT http://{{ .site }}/

NAME alarm
CONNECT http://{{ .site }}/alarm

NAME default_is_latest
EMIT hi
ACK

NAME explicit
EMIT hi
C root
ACK

NAME close_root
DISCONNECT
C root

NAME alarm_still_open
EMIT hi
ACK
`, nil)

	bodies := map[string]string{}
	for _, r := range sf.Results {
		bodies[r.Step.Name] = jsonString(r.Body)
		if r.StatusCode != 200 {
			t.Errorf("%s: %d %s", r.Step.Name, r.StatusCode, r.Error)
		}
	}
	if bodies["default_is_latest"] != `"hi on /alarm"` {
		t.Errorf("default connection should be the latest CONNECT, got %s", bodies["default_is_latest"])
	}
	if bodies["explicit"] != `"hi on /"` {
		t.Errorf("C should pick the connection, got %s", bodies["explicit"])
	}
	if bodies["alarm_still_open"] != `"hi on /alarm"` {
		t.Errorf("closing one connection should not close another, got %s", bodies["alarm_still_open"])
	}
}

func TestExecuteUsesPreviousResults(t *testing.T) {
	fs := newFakeServer(t)
	got := make(chan []any, 1)
	fs.onEvent = func(c *fakeConn, p packet) {
		name, args := eventName(p)
		if name == "add" {
			c.ack(p.Namespace, p.AckID, []any{map[string]any{"_id": "abc123"}})
		} else {
			got <- args
		}
	}

	runScript(t, fs, `
NAME conn
CONNECT http://{{ .site }}/

NAME added
EMIT add
ACK

NAME remove
EMIT remove
B {"_id": "{{ dig "added.0._id" }}"}
`, nil)

	if args := mustReceive(t, got, "remove"); jsonString(args) != `[{"_id":"abc123"}]` {
		t.Errorf("got %s", jsonString(args))
	}
}

func TestExecuteFailedConnectRecordsErrors(t *testing.T) {
	fs := newFakeServer(t)
	host := fs.host()
	fs.srv.Close()

	sf := runScript(t, nil, `
NAME conn
CONNECT http://{{ .site }}/

NAME emit
EMIT x
ACK

NAME wait
WAIT y

NAME collect
COLLECT 1ms

NAME bye
DISCONNECT
`, map[string]any{"site": host})

	for _, r := range sf.Results {
		if r.StatusCode != StatusError || r.Error == "" {
			t.Errorf("%s: expected recorded error got %d %q", r.Step.Name, r.StatusCode, r.Error)
		}
	}
}

func TestExecuteHTTPSteps(t *testing.T) {
	var gotBody string
	var gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"identifier": "id-1"}`))
	}))
	defer api.Close()

	sf := runScript(t, nil, `
NAME value
SET hello

NAME create
POST {{ .api }}/api/v3/entries
H Authorization: Bearer {{ .value }}
B {"sgv": 100}

NAME uses_it
SET {{ dig "create.identifier" }}
`, map[string]any{"api": api.URL})

	if gotBody != `{"sgv": 100}` || gotAuth != "Bearer hello" {
		t.Errorf("http request body %q auth %q", gotBody, gotAuth)
	}
	if sf.Results[1].StatusCode != 201 || sf.Results[1].HTTP == nil {
		t.Errorf("http result %+v", sf.Results[1])
	}
	if sf.CTX["uses_it"] != "id-1" {
		t.Errorf("SET from http response got %v", sf.CTX["uses_it"])
	}
}

func TestExecuteErrors(t *testing.T) {
	fs := newFakeServer(t)
	cases := map[string]string{
		"missing template key": "NAME c\nCONNECT http://{{ .nope }}/\n",
		"body not JSON":        "NAME c\nCONNECT http://{{ .site }}/\nNAME e\nEMIT x\nB {not json}\n",
		"bad body template":    "NAME c\nCONNECT http://{{ .site }}/\nNAME e\nEMIT x\nB {{ .nope }}\n",
		"bad header template":  "NAME c\nCONNECT http://{{ .site }}/\nH X: {{ .nope }}\n",
		"http unreachable":     "NAME h\nGET http://127.0.0.1:1/\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			steps, err := parseScript(strings.NewReader(script))
			if err != nil {
				t.Fatal(err)
			}
			sf := &ScriptFile{Name: "x", Steps: steps, CTX: map[string]any{"site": fs.host()}}
			if err := sf.Execute(&http.Client{}); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestWriteResultsToFolder(t *testing.T) {
	fs := newFakeServer(t)
	fs.onEvent = func(c *fakeConn, p packet) { c.ack(p.Namespace, p.AckID, "ok") }
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[1,2]`))
	}))
	defer api.Close()

	sf := runScript(t, fs, `
NAME Root Conn
CONNECT http://{{ .site }}/
H X-Test: yes

NAME emit
EMIT authorize
B {"token": "t"}
ACK

NAME storage
CONNECT http://{{ .site }}/storage

NAME wait
WAIT never
T 10ms

NAME http
GET {{ .api }}/api/v1/entries

NAME all
COLLECT 1ms
C Root Conn
`, map[string]any{"api": api.URL})

	t.Chdir(t.TempDir())
	if err := sf.WriteResultsToFolder("svc"); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob("results/svc/test_script/*.json")
	names := []string{}
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	want := "001.root-conn.json 002.emit.json 003.storage.json 004.wait.json 005.http.json 006.all.json"
	if strings.Join(names, " ") != want {
		t.Fatalf("files %v want %s", names, want)
	}

	read := func(name string) map[string]any {
		raw, err := os.ReadFile("results/svc/test_script/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	host := fs.host()
	checks := []struct {
		file, method, url, ns, event string
		status                       float64
	}{
		{"001.root-conn.json", "CONNECT", "http://" + host + "/socket.io/connect", "/", "connect", 200},
		{"002.emit.json", "EMIT", "http://" + host + "/socket.io/authorize", "/", "authorize", 200},
		{"003.storage.json", "CONNECT", "http://" + host + "/socket.io/storage/connect", "/storage", "connect", 200},
		{"004.wait.json", "WAIT", "http://" + host + "/socket.io/storage/never", "/storage", "never", 408},
		{"006.all.json", "COLLECT", "http://" + host + "/socket.io/events", "/", "events", 200},
	}
	for _, c := range checks {
		m := read(c.file)
		req := m["request"].(map[string]any)
		if req["method"] != c.method || req["url"] != c.url || req["namespace"] != c.ns || req["event"] != c.event || m["statusCode"] != c.status {
			t.Errorf("%s: got %s %s %s %s %v", c.file, req["method"], req["url"], req["namespace"], req["event"], m["statusCode"])
		}
		if _, ok := m["durationMs"]; !ok {
			t.Errorf("%s: missing durationMs", c.file)
		}
	}

	emit := read("002.emit.json")
	if jsonString(emit["request"].(map[string]any)["args"]) != `[{"token":"t"}]` || emit["body"] != "ok" || emit["request"].(map[string]any)["ack"] != true {
		t.Errorf("emit result %v", emit)
	}
	if wait := read("004.wait.json"); !strings.Contains(wait["error"].(string), "timed out") {
		t.Errorf("wait error %v", wait["error"])
	}
	if conn := read("001.root-conn.json"); jsonString(conn["request"].(map[string]any)["headers"]) != `{"X-Test":["yes"]}` {
		t.Errorf("connect headers %v", conn["request"])
	}

	// HTTP steps are saved in the HTTP result format
	httpRes := read("005.http.json")
	if httpRes["statusCode"] != 200.0 || jsonString(httpRes["body"]) != `[1,2]` || httpRes["request"].(map[string]any)["method"] != "GET" {
		t.Errorf("http result %v", httpRes)
	}
}

func TestUnwrapArgs(t *testing.T) {
	cases := []struct {
		in   []any
		want string
	}{
		{nil, "null"},
		{[]any{}, "null"},
		{[]any{nil}, "null"},
		{[]any{"a"}, `"a"`},
		{[]any{[]any{1}}, `[1]`},
		{[]any{1, 2}, `[1,2]`},
	}
	for _, c := range cases {
		if got := jsonString(unwrapArgs(c.in)); got != c.want {
			t.Errorf("unwrapArgs(%v) = %s want %s", c.in, got, c.want)
		}
	}
}

func TestSavedRequestForBadConnectURL(t *testing.T) {
	r := savedRequestFor(Result{Step: Step{Verb: VerbWait, Target: "x", Conn: "c"}}, "://bad")
	if r.URL != "" || r.Method != "WAIT" || r.Event != "x" {
		t.Errorf("got %+v", r)
	}
}

func TestSavedRequestForEscapesEventNames(t *testing.T) {
	r := savedRequestFor(Result{Step: Step{Verb: VerbWait, Target: "what? #now", Conn: "c"}}, "http://host:1/storage")
	if r.Path != "/socket.io/storage/what? #now" {
		t.Errorf("path %q", r.Path)
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Path != r.Path || u.RawQuery != "" || u.Fragment != "" {
		t.Errorf("url %q should round trip to the path, got %+v %v", r.URL, u, err)
	}
}
