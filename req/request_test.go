package req

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, s string) []Request {
	t.Helper()
	requests, err := ParseRequests(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return requests
}

func TestParseRequests(t *testing.T) {
	requests := parse(t, `
# comment
// another comment
NAME create
POST http://{{ .site }}/api/v3/entries
H Authorization: Bearer {{ jwt }}
H X-Time: 10:20:30
B {"sgv": 100,
  "type": "sgv"}

NAME value
SET {{ .x }}

NAME get
GET http://{{ .site }}/api/v1/entries`)

	if len(requests) != 3 {
		t.Fatalf("got %d requests", len(requests))
	}

	r := requests[0]
	if r.Name != "create" || r.Method != "POST" || r.URL != "http://{{ .site }}/api/v3/entries" || r.IsMethod {
		t.Errorf("request %+v", r)
	}
	if r.Headers.Get("Authorization") != "Bearer {{ jwt }}" || r.Headers.Get("X-Time") != "10:20:30" {
		t.Errorf("headers %v", r.Headers)
	}
	if r.Body != "{\"sgv\": 100,\n\"type\": \"sgv\"}" {
		t.Errorf("body %q", r.Body)
	}

	if s := requests[1]; s.Method != "SET" || !s.IsMethod || s.URL != "{{ .x }}" {
		t.Errorf("set %+v", s)
	}
	if g := requests[2]; g.Method != "GET" || g.Body != "" {
		t.Errorf("get without trailing newline %+v", g)
	}
}

func TestParseRequestsAllMethods(t *testing.T) {
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		r := parse(t, "NAME a\n"+m+" http://x/\n")
		if r[0].Method != m {
			t.Errorf("method %s parsed as %s", m, r[0].Method)
		}
	}
}

func TestParseRequestsBoundaries(t *testing.T) {
	bad := map[string]string{
		"request before NAME":   "GET http://x/\n",
		"header before NAME":    "H X: y\n",
		"body before NAME":      "B {}\n",
		"SET before NAME":       "SET x\n",
		"empty NAME":            "NAME \nGET http://x/\n",
		"unknown method":        "NAME a\nHEAD http://x/\n",
		"lowercase method":      "NAME a\nget http://x/\n",
		"header without colon":  "NAME a\nGET http://x/\nH broken\n",
		"unknown line":          "NAME a\nGET http://x/\nWHAT\n",
		"method without url":    "NAME a\nGET\n",
		"continuation not body": "NAME a\nGET http://x/\n  indented\n",
	}
	for name, s := range bad {
		if _, err := ParseRequests(strings.NewReader(s)); err == nil {
			t.Errorf("%s: expected error parsing %q", name, s)
		}
	}

	if r := parse(t, ""); len(r) != 0 {
		t.Errorf("empty input gave %v", r)
	}
	if r := parse(t, "\n\n# only comments\n"); len(r) != 0 {
		t.Errorf("comment only input gave %v", r)
	}

	// a NAME with no request line is still parsed, ValidateRequests rejects it
	if r := parse(t, "NAME a\n"); len(r) != 1 || r[0].Method != "" {
		t.Errorf("name only %+v", r)
	}

	// the last B wins
	if r := parse(t, "NAME a\nPOST http://x/\nB 1\nB 2\n"); r[0].Body != "2" {
		t.Errorf("body %q", r[0].Body)
	}

	// windows line endings
	if r := parse(t, "NAME a\r\nGET http://x/\r\nH X: y\r\n"); r[0].URL != "http://x/" || r[0].Headers.Get("X") != "y" {
		t.Errorf("crlf %+v", r[0])
	}

	// repeated headers are kept
	if r := parse(t, "NAME a\nGET http://x/\nH X: 1\nH X: 2\n"); len(r[0].Headers.Values("X")) != 2 {
		t.Errorf("headers %v", r[0].Headers)
	}

	// header with an empty value
	if r := parse(t, "NAME a\nGET http://x/\nH X:\n"); r[0].Headers.Get("X") != "" || len(r[0].Headers.Values("X")) != 1 {
		t.Errorf("empty header %v", r[0].Headers)
	}

	// a blank line inside a multi-line body is skipped, the body continues
	if r := parse(t, "NAME a\nPOST http://x/\nB [1,\n\n  2]\n"); r[0].Body != "[1,\n2]" {
		t.Errorf("body with blank line %q", r[0].Body)
	}
}

func TestParseRequestFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "v1_things.http")
	os.WriteFile(file, []byte("NAME a\nGET http://x/\n"), 0o644)

	ctx := map[string]any{"k": 1}
	rf, err := ParseRequestFile(file, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rf.Name != "v1_things" || len(rf.Requests) != 1 || rf.CTX["k"] != 1 {
		t.Errorf("request file %+v", rf)
	}

	if _, err := ParseRequestFile(filepath.Join(dir, "missing.http"), ctx); err == nil {
		t.Error("expected error for a missing file")
	}

	os.WriteFile(file, []byte("WHAT\n"), 0o644)
	if _, err := ParseRequestFile(file, ctx); err == nil {
		t.Error("expected parse error")
	}
}

func TestRenderTemplate(t *testing.T) {
	ctx := map[string]any{
		"site":       "localhost:1337",
		"token":      "tok",
		"read_token": "rtok",
		"jwt":        "j",
		"read_jwt":   "rj",
		"create":     map[string]any{"identifier": "id-1", "list": []any{"a", map[string]any{"b": 2.0}}},
		"subjects":   []any{map[string]any{"name": "read", "accessToken": "r-1"}, map[string]any{"name": "write", "accessToken": "w-1"}},
	}

	cases := map[string]string{
		"":                                    "",
		"plain text":                          "plain text",
		"{{ .site }}":                         "localhost:1337",
		"{{ token }} {{ read_token }}":        "tok rtok",
		"{{ jwt }} {{ read_jwt }}":            "j rj",
		`{{ dig "create.identifier" }}`:       "id-1",
		`{{ dig "create.list.0" }}`:           "a",
		`{{ dig "create.list.1.b" }}`:         "2",
		`{{ dig "create.missing" }}`:          "<no value>",
		`{{ dig "create.list.5" }}`:           "<no value>",
		`{{ dig "create.list.-1" }}`:          "<no value>",
		`{{ dig "create.list.x" }}`:           "<no value>",
		`{{ dig "create.identifier.deep" }}`:  "<no value>",
		`{{ dig "" }}`:                        "<no value>",
		`{{ tokenByName .subjects "write" }}`: "w-1",
		`{{ tokenByName .subjects "nope" }}`:  "",
	}
	for in, want := range cases {
		got, err := RenderTemplate(in, ctx)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %q want %q", in, got, want)
		}
	}

	errs := []string{
		"{{ .missing }}",
		"{{ .site",
		"{{ unknownFunc }}",
		`{{ time "x" }}`,
	}
	for _, in := range errs {
		if _, err := RenderTemplate(in, ctx); err == nil {
			t.Errorf("expected error rendering %q", in)
		}
	}

	// token funcs fail with an error, not a panic, before the setup file has set them
	for _, in := range []string{"{{ token }}", "{{ read_token }}", "{{ jwt }}", "{{ read_jwt }}"} {
		if _, err := RenderTemplate(in, map[string]any{}); err == nil {
			t.Errorf("expected error rendering %q without context", in)
		}
	}
}

func TestTemplateTime(t *testing.T) {
	before := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	got, err := RenderTemplate("{{ time 5 }}", nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("time is not RFC3339: %q", got)
	}
	after := time.Now().Add(-5 * time.Minute)
	if parsed.Before(before) || parsed.After(after) {
		t.Errorf("time 5 = %v, want between %v and %v", parsed, before, after)
	}

	// negative offsets are in the future
	future, _ := RenderTemplate("{{ time -60 }}", nil)
	if p, _ := time.Parse(time.RFC3339, future); !p.After(time.Now()) {
		t.Errorf("time -60 = %s should be in the future", future)
	}

	ms := templateITime(0)
	if now := time.Now().UnixMilli(); ms > now || now-ms > 1000 {
		t.Errorf("itime 0 = %d, now %d", ms, now)
	}
	if templateITime(1)-templateITime(0) > -59000 {
		t.Errorf("itime 1 should be a minute before itime 0")
	}
}

func TestTemplateDig(t *testing.T) {
	root := map[string]any{"a": []any{[]any{"x"}}, "n": nil}
	if templateDig(root, "a.0.0") != "x" {
		t.Error("nested arrays")
	}
	if templateDig(root, "n") != nil || templateDig(root, "n.deeper") != nil {
		t.Error("nil values")
	}
	if templateDig(nil, "a") != nil || templateDig("string", "a") != nil {
		t.Error("non map roots")
	}
}

func TestBuildRealRequest(t *testing.T) {
	ctx := map[string]any{"site": "h", "jwt": "j"}
	r := parse(t, "NAME a\nPOST http://{{ .site }}/x\nH Authorization: Bearer {{ jwt }}\nB {\"s\": \"{{ .site }}\"}\n")[0]

	built, err := r.BuildRealRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if built.URL != "http://h/x" || built.Body != `{"s": "h"}` || built.Headers.Get("Authorization") != "Bearer j" {
		t.Errorf("built %+v", built)
	}
	if built.Headers.Get("Content-Type") != "application/json" || built.Headers.Get("Accept") != "application/json" {
		t.Errorf("default headers %v", built.Headers)
	}
	// the original request still has its template so it can be built again
	if r.URL != "http://{{ .site }}/x" || r.Headers.Get("Authorization") != "Bearer {{ jwt }}" {
		t.Errorf("original modified %+v", r)
	}

	// explicit content headers are kept
	r = parse(t, "NAME a\nPOST http://h/\nH Content-Type: text/plain\nH Accept: */*\n")[0]
	built, _ = r.BuildRealRequest(ctx)
	if built.Headers.Get("Content-Type") != "text/plain" || built.Headers.Get("Accept") != "*/*" {
		t.Errorf("explicit headers %v", built.Headers)
	}

	// SET renders its value and is not given HTTP headers
	r = parse(t, "NAME a\nSET {{ .site }}\n")[0]
	built, _ = r.BuildRealRequest(ctx)
	if built.URL != "h" || !built.IsMethod || built.Headers.Get("Content-Type") != "" {
		t.Errorf("set %+v", built)
	}

	for _, s := range []string{
		"NAME a\nGET http://{{ .nope }}/\n",
		"NAME a\nGET http://h/\nH X: {{ .nope }}\n",
		"NAME a\nPOST http://h/\nB {{ .nope }}\n",
		"NAME a\nSET {{ .nope }}\n",
	} {
		if _, err := parse(t, s)[0].BuildRealRequest(ctx); err == nil {
			t.Errorf("expected render error for %q", s)
		}
	}
}

func TestExecute(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Reply", "yes")
		w.WriteHeader(418)
		w.Write([]byte("teapot"))
	}))
	defer srv.Close()

	r := Request{Name: "a", Method: "PATCH", URL: srv.URL + "/p?q=1", Headers: http.Header{"X-Test": {"1"}}, Body: `{"a":1}`}
	resp, err := r.Execute(srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != "PATCH" || got.URL.Path != "/p" || got.URL.Query().Get("q") != "1" || got.Header.Get("X-Test") != "1" || gotBody != `{"a":1}` {
		t.Errorf("server got %s %s %v %q", got.Method, got.URL, got.Header, gotBody)
	}
	if resp.StatusCode != 418 || string(resp.Body) != "teapot" || resp.Headers.Get("X-Reply") != "yes" || resp.Name != "a" || resp.Request.URL != r.URL {
		t.Errorf("response %+v", resp)
	}

	if _, err := (Request{Name: "bad", Method: "GET", URL: "http://127.0.0.1:1/"}).Execute(srv.Client()); err == nil {
		t.Error("expected connection error")
	}
	if _, err := (Request{Name: "bad", Method: "GET", URL: "://bad"}).Execute(srv.Client()); err == nil {
		t.Error("expected url error")
	}
}

func TestExecuteMethods(t *testing.T) {
	resp, err := Request{Name: "v", Method: "SET", URL: "value", IsMethod: true}.Execute(nil)
	if err != nil || string(resp.Body) != "value" || resp.Name != "v" {
		t.Errorf("set %+v %v", resp, err)
	}
	if _, err := (Request{Name: "v", Method: "NOPE", URL: "x", IsMethod: true}).Execute(nil); err == nil {
		t.Error("expected unsupported method error")
	}
	if _, err := (Request{Name: "v", Method: "GET"}).executeMethod(); err == nil {
		t.Error("expected error for a non method request")
	}
}

func TestRequestFileExecuteChainsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/create":
			w.Write([]byte(`{"identifier": "abc"}`))
		case "/text":
			w.Write([]byte(`not json`))
		default:
			w.Write([]byte(r.URL.Path))
		}
	}))
	defer srv.Close()

	requests := parse(t, `
NAME create
POST {{ .base }}/create

NAME text
GET {{ .base }}/text

NAME fetch
GET {{ .base }}/things/{{ dig "create.identifier" }}

NAME saved
SET {{ .fetch }}
`)
	rf := &RequestFile{Name: "f", Requests: requests, CTX: map[string]any{"base": srv.URL}}
	if err := rf.Execute(srv.Client()); err != nil {
		t.Fatal(err)
	}
	if rf.CTX["text"] != "not json" {
		t.Errorf("non JSON body should be stored as a string, got %v", rf.CTX["text"])
	}
	if rf.CTX["fetch"] != "/things/abc" || rf.CTX["saved"] != "/things/abc" {
		t.Errorf("chained context %v %v", rf.CTX["fetch"], rf.CTX["saved"])
	}
	if len(rf.Responses) != 4 {
		t.Errorf("got %d responses", len(rf.Responses))
	}

	// rendering errors stop the file
	rf = &RequestFile{Name: "f", Requests: parse(t, "NAME a\nGET {{ .missing }}\n"), CTX: map[string]any{}}
	if err := rf.Execute(srv.Client()); err == nil {
		t.Error("expected render error")
	}
}

func TestSetContext(t *testing.T) {
	ctx := map[string]any{}
	SetContext(ctx, "obj", []byte(`{"a": 1}`))
	SetContext(ctx, "arr", []byte(`[1]`))
	SetContext(ctx, "num", []byte(`12`))
	SetContext(ctx, "str", []byte(`"quoted"`))
	SetContext(ctx, "text", []byte(`hello`))
	SetContext(ctx, "empty", nil)
	SetContext(ctx, "null", []byte(`null`))

	want := map[string]string{
		"obj":   `{"a":1}`,
		"arr":   `[1]`,
		"num":   `12`,
		"str":   `"quoted"`,
		"text":  `"hello"`,
		"empty": `""`,
		"null":  `null`,
	}
	for k, w := range want {
		got, _ := json.Marshal(ctx[k])
		if string(got) != w {
			t.Errorf("%s = %s want %s", k, got, w)
		}
	}
	if _, ok := ctx["null"]; !ok {
		t.Error("a null body should still set the key")
	}
}

func TestSaveResponse(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, resp Response) map[string]any {
		t.Helper()
		f := filepath.Join(dir, name)
		if err := SaveResponse(f, resp); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(f)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s is not JSON: %s", name, raw)
		}
		if strings.Contains(string(raw), `\u0026`) {
			t.Errorf("& should not be escaped: %s", raw)
		}
		return m
	}

	m := write("json.json", Response{
		Request:    Request{Method: "GET", URL: "http://h/x?a=1&b=2", Body: `{"q":1}`},
		StatusCode: 200,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       []byte(`{"a": [1, "&"]}`),
		Duration:   1500 * time.Millisecond,
	})
	req := m["request"].(map[string]any)
	if req["url"] != "http://h/x?a=1&b=2" || req["method"] != "GET" {
		t.Errorf("request %v", req)
	}
	if b, _ := json.Marshal(req["body"]); string(b) != `{"q":1}` {
		t.Errorf("request body %s", b)
	}
	if a := m["body"].(map[string]any)["a"].([]any); len(a) != 2 || a[0] != 1.0 || a[1] != "&" {
		t.Errorf("body %v", m["body"])
	}
	if m["statusCode"] != 200.0 || m["durationMs"] != 1500.0 {
		t.Errorf("status/duration %v %v", m["statusCode"], m["durationMs"])
	}

	m = write("text.json", Response{Request: Request{Body: "not json"}, Body: []byte("<html>")})
	if m["body"] != "<html>" || m["request"].(map[string]any)["body"] != "not json" {
		t.Errorf("text bodies %v", m)
	}

	m = write("empty.json", Response{})
	if m["body"] != nil || m["request"].(map[string]any)["body"] != nil {
		t.Errorf("empty bodies should be null %v", m)
	}

	if err := SaveResponse(filepath.Join(dir, "no", "such", "dir.json"), Response{}); err == nil {
		t.Error("expected error writing to a missing folder")
	}
}

func TestWriteJSONFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.json")
	if err := WriteJSONFile(f, map[string]any{"u": "a&b<c>"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f)
	if !strings.Contains(string(raw), `"a&b\u003cc\u003e"`) {
		t.Errorf("only & is unescaped, got %s", raw)
	}
	if err := WriteJSONFile(f, map[string]any{"f": func() {}}); err == nil {
		t.Error("expected marshal error")
	}
}

func TestWriteResponsesToFolder(t *testing.T) {
	t.Chdir(t.TempDir())
	rf := &RequestFile{Name: "group", Responses: []Response{
		{Name: "First One", StatusCode: 200},
		{Name: "second/../../escape", StatusCode: 201},
	}}
	if err := rf.WriteResponsesToFolder("svc"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"results/svc/group/001.first-one.json", "results/svc/group/002.secondescape.json"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s", f)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"simple_name":      "simple_name",
		"Mixed Case Name":  "mixed-case-name",
		"a/b\\c:d*e?f":     "abcdef",
		"../../etc/passwd": "etcpasswd",
		"dots.and,commas":  "dotsandcommas",
		"tab\tnewline\n":   "tabnewline",
		"ünïcödé":          "ünïcödé",
		"123-abc":          "123-abc",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q want %q", in, got, want)
		}
	}
}

func TestValidateRequests(t *testing.T) {
	file := func(s string) *RequestFile {
		return &RequestFile{Requests: parse(t, s)}
	}

	ok := []*RequestFile{file("NAME a\nGET http://x/\nNAME b\nSET 1\n"), file("NAME c\nPOST http://x/\n")}
	if err := ValidateRequests(ok, map[string]any{"good_name": 1}); err != nil {
		t.Errorf("valid requests: %v", err)
	}
	if err := ValidateRequests(nil, nil); err != nil {
		t.Errorf("no requests: %v", err)
	}

	bad := map[string]struct {
		files []*RequestFile
		ctx   map[string]any
	}{
		"duplicate in a file":   {[]*RequestFile{file("NAME a\nGET http://x/\nNAME a\nGET http://x/\n")}, nil},
		"duplicate across":      {[]*RequestFile{file("NAME a\nGET http://x/\n"), file("NAME a\nGET http://x/\n")}, nil},
		"clashes with context":  {[]*RequestFile{file("NAME site\nSET x\n")}, map[string]any{"site": 1}},
		"missing method":        {[]*RequestFile{file("NAME a\n")}, nil},
		"empty name":            {[]*RequestFile{{Requests: []Request{{Method: "GET", URL: "x"}}}}, nil},
		"missing url":           {[]*RequestFile{{Requests: []Request{{Name: "a", Method: "GET"}}}}, nil},
		"context name dash":     {nil, map[string]any{"bad-name": 1}},
		"context name digit":    {nil, map[string]any{"1abc": 1}},
		"context name dot":      {nil, map[string]any{"a.b": 1}},
		"context name empty":    {nil, map[string]any{"": 1}},
		"context name unicode":  {nil, map[string]any{"ñ": 1}},
		"context name with spc": {nil, map[string]any{"a b": 1}},
	}
	for name, c := range bad {
		if err := ValidateRequests(c.files, c.ctx); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
