package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decode(t testing.TB, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON %q: %v", s, err)
	}
	return v
}

func diffStrings(d *diffs) []string {
	out := []string{}
	for _, x := range d.list() {
		out = append(out, x.String())
	}
	return out
}

func TestEndpointPath(t *testing.T) {
	cases := map[string]string{
		"http://h:1337/api/v1/entries":                  "/api/v1/entries",
		"http://h/api/v1/entries.json":                  "/api/v1/entries",
		"http://h/api/v1/entries/":                      "/api/v1/entries",
		"http://h/api/v1/entries.json?count=10&find=1":  "/api/v1/entries",
		"http://h/api/v3/entries/abc-123":               "/api/v3/entries",
		"http://h/api/v3/entries/abc/history/1":         "/api/v3/entries",
		"http://h/api/v1/entries/sgv.json":              "/api/v1/entries",
		"http://h/api/v1/count/entries/where":           "/api/v1/count",
		"http://h/api/v1":                               "/api/v1",
		"http://h/":                                     "/",
		"http://h/socket.io/authorize":                  "/socket.io/authorize",
		"http://h/socket.io/storage/create":             "/socket.io/storage/create",
		"http://h/socket.io/what%3F%20%23now":           "/socket.io/what? #now",
		"http://h/api/v1/entries#fragment":              "/api/v1/entries",
		"http://h":                                      "",
		"":                                              "",
		"localhost:1337":                                "",
		"://bad":                                        "",
		"http://h/a.json/b":                             "/a.json/b",
		"http://h/api/v3/lastModified.json":             "/api/v3/lastModified",
		"http://h/api/v2/authorization/request/token=x": "/api/v2/authorization",
	}
	for in, want := range cases {
		if got := EndpointPath(in); got != want {
			t.Errorf("EndpointPath(%q) = %q want %q", in, got, want)
		}
	}
}

func TestStatusScore(t *testing.T) {
	cases := []struct {
		a, b int
		want float64
	}{
		{200, 200, 100},
		{0, 0, 100},
		{404, 404, 100},
		{200, 201, 80},
		{400, 404, 80},
		{500, 503, 80},
		{301, 302, 80},
		{200, 400, 0},
		{201, 500, 0},
		{200, 301, 0},
		{200, 0, 0},
		{400, 200, 20},
		{500, 201, 20},
		{301, 200, 20},
		{101, 200, 20},
		{0, 200, 0},
		{600, 200, 0},
		{-1, 200, 0},
		{400, 500, 0},
		{500, 400, 0},
		{408, 500, 0},
		{99, 100, 0},
	}
	for _, c := range cases {
		if got := StatusScore(c.a, c.b); got != c.want {
			t.Errorf("StatusScore(%d, %d) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareScores(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64
	}{
		{"null null", `null`, `null`, 100},
		{"reference null other has data", `null`, `{"a":1}`, extraDataScore},
		{"reference data other null", `{"a":1}`, `null`, 0},
		{"same string", `"a"`, `"a"`, 100},
		{"different string", `"a"`, `"b"`, valueMismatchScore},
		{"empty strings", `""`, `""`, 100},
		{"same number", `1`, `1`, 100},
		{"different number", `1`, `2`, valueMismatchScore},
		{"zero vs negative zero", `0`, `-0`, 100},
		{"int vs float same value", `1`, `1.0`, 100},
		{"same bool", `true`, `true`, 100},
		{"different bool", `true`, `false`, boolMismatchScore},
		{"number vs numeric string", `100`, `"100"`, 0},
		{"string vs number", `"100"`, `100`, 0},
		{"bool vs string", `true`, `"true"`, 0},
		{"bool vs number", `false`, `0`, 0},
		{"object vs array", `{}`, `[]`, 0},
		{"array vs object", `[]`, `{}`, 0},
		{"number vs object", `1`, `{}`, 0},
		{"empty objects", `{}`, `{}`, 100},
		{"empty reference object, other has keys", `{}`, `{"a":1}`, extraDataScore},
		{"object loses every key", `{"a":1,"b":2}`, `{}`, 0},
		{"identical object", `{"a":1,"b":"x"}`, `{"a":1,"b":"x"}`, 100},
		{"one of two keys missing", `{"a":1,"b":2}`, `{"a":1}`, 50},
		{"one extra key", `{"a":1}`, `{"a":1,"b":2}`, 100 / (1 + extraKeyWeight)},
		{"completely different keys", `{"a":1}`, `{"b":1}`, 0},
		{"values differ only", `{"_id":"x","date":1}`, `{"_id":"y","date":2}`, valueMismatchScore},
		{"nested type change", `{"a":{"b":1}}`, `{"a":{"b":"1"}}`, 0},
		{"nested key missing", `{"a":{"b":1,"c":1}}`, `{"a":{"b":1}}`, 50},
		{"empty arrays", `[]`, `[]`, 100},
		{"reference empty array other has items", `[]`, `[1]`, extraDataScore},
		{"other returns no items", `[1,2]`, `[]`, 0},
		{"identical arrays", `[1,2,3]`, `[1,2,3]`, 100},
		{"array half the length", `[1,1]`, `[1]`, 75},
		{"array double the length", `[1]`, `[1,1]`, 75},
		{"arrays differ by position value", `[1,2]`, `[2,1]`, valueMismatchScore},
		{"array of objects missing a key", `[{"a":1,"b":1},{"a":1,"b":1}]`, `[{"a":1},{"a":1}]`, 50},
		{"array item wrong type", `[1,"a"]`, `[1,2]`, 50},
		{"nested arrays", `[[1],[2]]`, `[[1],[2]]`, 100},
	}
	for _, c := range cases {
		got := compare(decode(t, c.a), decode(t, c.b), "$", newDiffs())
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: compare(%s, %s) = %v want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestCompareDifferences(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want []string
	}{
		{"identical", `{"a":[1,{"b":true}]}`, `{"a":[1,{"b":true}]}`, []string{}},
		{"values only are not reported", `{"id":"x","n":1}`, `{"id":"y","n":2}`, []string{}},
		{"type", `{"sgv":100}`, `{"sgv":"100"}`, []string{"type $.sgv: number vs string"}},
		{"root type", `[]`, `{}`, []string{"type $: array vs object"}},
		{"missing then extra", `{"a":1,"b":1}`, `{"a":1,"c":1}`, []string{"missing $.b", "extra $.c"}},
		{"bool value", `{"success":true}`, `{"success":false}`, []string{"value $.success: true vs false"}},
		{"null other", `{"a":{"b":1}}`, `{"a":null}`, []string{"null $.a: object vs null"}},
		{"null reference", `{"a":null}`, `{"a":[1]}`, []string{"null $.a: null vs array"}},
		{"length", `[1,2,3]`, `[1]`, []string{"length $: 3 vs 1"}},
		{
			"array element differences are merged",
			`[{"a":1,"b":1},{"a":1,"b":1},{"a":1,"b":1}]`,
			`[{"a":1},{"a":1},{"a":"1"}]`,
			[]string{"missing $[].b (x3)", "type $[].a: number vs string"},
		},
		{"nested arrays", `{"x":[[{"k":1}]]}`, `{"x":[[{}]]}`, []string{"missing $.x[][].k"}},
		{"keys are sorted", `{"z":1,"m":1,"a":1}`, `{}`, []string{"missing $.a", "missing $.m", "missing $.z"}},
		{"dotted keys", `{"a.b":1}`, `{}`, []string{"missing $.a.b"}},
		{"empty key", `{"":1}`, `{}`, []string{"missing $."}},
	}
	for _, c := range cases {
		d := newDiffs()
		compare(decode(t, c.a), decode(t, c.b), "$", d)
		got := diffStrings(d)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func TestBodyScoreTextReference(t *testing.T) {
	d := newDiffs()
	// a text reference (e.g. "Unauthorized") with a JSON error from the other service is fine
	if got := BodyScore("Unauthorized", decode(t, `{"message":"Unauthorized"}`), d); got != 100 {
		t.Errorf("text vs JSON object = %v", got)
	}
	if got := BodyScore("x", decode(t, `[1]`), nil); got != 100 {
		t.Errorf("text vs JSON array = %v", got)
	}
	if diffs := diffStrings(d); len(diffs) != 1 || !strings.HasPrefix(diffs[0], `format $: text "Unauthorized" vs JSON object`) {
		t.Errorf("format difference %q", diffs)
	}

	// but not the other way around, clients parsing JSON break on text
	if got := BodyScore(decode(t, `{"message":"x"}`), "x", nil); got != 0 {
		t.Errorf("JSON vs text = %v", got)
	}
	// text vs text is a value comparison
	if got := BodyScore("a", "b", nil); got != valueMismatchScore {
		t.Errorf("text vs text = %v", got)
	}
	// long text is truncated in the difference
	d = newDiffs()
	BodyScore(strings.Repeat("x", 500), map[string]any{}, d)
	if s := diffStrings(d)[0]; len(s) > 100 {
		t.Errorf("difference not truncated: %d chars", len(s))
	}
}

func TestCompareResults(t *testing.T) {
	r1 := Result{Method: "GET", Endpoint: "/api/v1/entries", Status: 200, DurationMs: 10, Body: decode(t, `[{"sgv":1}]`), ContentType: "application/json; charset=utf-8"}

	same := CompareResults(r1, r1)
	if same.Score != 100 || same.StatusScore != 100 || same.BodyScore != 100 || len(same.Differences) != 0 {
		t.Errorf("identical results %+v", same)
	}
	if !same.HasDuration || same.DurationDiff != 0 {
		t.Errorf("duration %+v", same)
	}

	r2 := r1
	r2.Status, r2.DurationMs, r2.ContentType = 201, 4, "application/json"
	tr := CompareResults(r1, r2)
	if tr.StatusScore != 80 || tr.DurationDiff != -6 || !tr.HasDuration {
		t.Errorf("201 vs 200 %+v", tr)
	}
	if tr.Score != statusWeight*80+bodyWeight*100 {
		t.Errorf("weighted score %v", tr.Score)
	}
	// charset alone is not a content type difference
	if len(tr.Differences) != 1 || tr.Differences[0].String() != "status: 200 vs 201" {
		t.Errorf("differences %v", tr.Differences)
	}

	r2 = r1
	r2.ContentType = "text/html"
	if tr := CompareResults(r1, r2); len(tr.Differences) != 1 || tr.Differences[0].String() != "content-type: application/json vs text/html" {
		t.Errorf("content type difference %v", tr.Differences)
	}
	r2.ContentType = ""
	if tr := CompareResults(r1, r2); len(tr.Differences) != 0 {
		t.Errorf("a missing content type is not compared, got %v", tr.Differences)
	}
}

func TestCompareResultsDurationOnlyWhenBothSucceed(t *testing.T) {
	cases := []struct {
		s1, s2 int
		want   bool
	}{
		{200, 200, true},
		{201, 200, true},
		{204, 299, true},
		{200, 500, false},
		{500, 200, false},
		{404, 201, false}, // was counted before because of operator precedence
		{200, 404, false},
		{0, 0, false},
		{300, 300, false},
	}
	for _, c := range cases {
		tr := CompareResults(Result{Status: c.s1, DurationMs: 5}, Result{Status: c.s2, DurationMs: 9})
		if tr.HasDuration != c.want {
			t.Errorf("%d vs %d HasDuration = %v", c.s1, c.s2, tr.HasDuration)
		}
		if !c.want && tr.DurationDiff != 0 {
			t.Errorf("%d vs %d DurationDiff = %d", c.s1, c.s2, tr.DurationDiff)
		}
	}
}

func TestMediaType(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"application/json":                "application/json",
		"Application/JSON; charset=UTF-8": "application/json",
		"text/plain;charset=utf-8":        "text/plain",
		"broken;;;":                       "broken",
		" text/html ; x":                  "text/html",
	}
	for in, want := range cases {
		if got := mediaType(in); got != want {
			t.Errorf("mediaType(%q) = %q want %q", in, got, want)
		}
	}
}

func writeResult(t *testing.T, root, rel string, v any) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if s, ok := v.(string); ok {
		data = []byte(s)
	} else {
		data, _ = json.Marshal(v)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func result(method, url string, status int, body any) map[string]any {
	return map[string]any{
		"request":    map[string]any{"method": method, "url": url},
		"statusCode": status,
		"durationMs": 3,
		"headers":    map[string]any{"content-type": []string{"application/json"}},
		"body":       body,
	}
}

func TestLoadResult(t *testing.T) {
	dir := t.TempDir()
	writeResult(t, dir, "ok.json", result("GET", "http://h/api/v1/entries.json?count=1", 200, []any{1.0}))
	r, err := LoadResult(filepath.Join(dir, "ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "GET" || r.Endpoint != "/api/v1/entries" || r.Status != 200 || r.DurationMs != 3 || r.ContentType != "application/json" {
		t.Errorf("result %+v", r)
	}

	writeResult(t, dir, "noduration.json", `{"request":{"method":"GET","url":"http://h/x"},"statusCode":0}`)
	if r, err := LoadResult(filepath.Join(dir, "noduration.json")); err != nil || r.DurationMs != 0 || r.Status != 0 {
		t.Errorf("status 0 without duration %+v %v", r, err)
	}

	bad := map[string]string{
		"invalid.json":   `{not json`,
		"norequest.json": `{"statusCode":200}`,
		"nostatus.json":  `{"request":{"method":"GET","url":"http://h/x"}}`,
		"array.json":     `[]`,
		"badstatus.json": `{"request":{},"statusCode":"200"}`,
	}
	for name, content := range bad {
		writeResult(t, dir, name, content)
		if _, err := LoadResult(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	if _, err := LoadResult(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Errorf("missing file should be IsNotExist, got %v", err)
	}
}

func TestCompareFolders(t *testing.T) {
	root := t.TempDir()
	ns := filepath.Join(root, "nightscout")
	other := filepath.Join(root, "other")

	// compared
	writeResult(t, ns, "v1_entries/001.get.json", result("GET", "http://a/api/v1/entries", 200, []any{map[string]any{"sgv": 1.0}}))
	writeResult(t, other, "v1_entries/001.get.json", result("GET", "http://b/api/v1/entries", 200, []any{map[string]any{"sgv": "1"}}))
	// missing in the other service
	writeResult(t, ns, "v1_entries/002.post.json", result("POST", "http://a/api/v1/entries", 200, nil))
	// setup file named after the reference service is skipped
	writeResult(t, ns, "nightscout/001.token.json", result("POST", "http://a/api/v2/authorization/subjects", 200, nil))
	// SET steps are skipped
	writeResult(t, ns, "v1_entries/003.value.json", result("SET", "localhost:1337", 0, "x"))
	writeResult(t, other, "v1_entries/003.value.json", result("SET", "localhost:1337", 0, "y"))
	// bad requests are skipped unless asked for
	writeResult(t, ns, "x_bad_requests/001.bad.json", result("POST", "http://a/api/v1/entries", 400, nil))
	writeResult(t, other, "x_bad_requests/001.bad.json", result("POST", "http://b/api/v1/entries", 200, nil))
	// not results
	writeResult(t, ns, "v1_entries/notes.txt", "hello")
	writeResult(t, ns, "loose.json", result("GET", "http://a/api/v1/x", 200, nil))
	writeResult(t, ns, "a/b/deep.json", result("GET", "http://a/api/v1/x", 200, nil))
	writeResult(t, ns, "v1_entries/004.broken.json", "{")
	// unreadable in the other service counts as missing
	writeResult(t, ns, "v1_entries/005.get.json", result("GET", "http://a/api/v1/entries", 200, nil))
	writeResult(t, other, "v1_entries/005.get.json", "{")
	// socket results
	writeResult(t, ns, "socket_v3_storage/001.c.json", result("CONNECT", "http://a/socket.io/storage/connect", 200, map[string]any{"sid": "1"}))
	writeResult(t, other, "socket_v3_storage/001.c.json", result("CONNECT", "http://b/socket.io/storage/connect", 500, nil))

	results, missing, err := CompareFolders(ns+string(os.PathSeparator), other, Options{})
	if err != nil {
		t.Fatal(err)
	}

	names := []string{}
	for _, r := range results {
		names = append(names, r.Name())
	}
	if strings.Join(names, ",") != "socket_v3_storage/001.c,v1_entries/001.get" {
		t.Errorf("compared %v", names)
	}
	missingNames := []string{}
	for _, m := range missing {
		missingNames = append(missingNames, m.Name)
	}
	if strings.Join(missingNames, ",") != "v1_entries/002.post,v1_entries/005.get" {
		t.Errorf("missing %v", missingNames)
	}

	for _, r := range results {
		switch r.Name() {
		case "v1_entries/001.get":
			if r.StatusScore != 100 || r.BodyScore != 0 || len(r.Differences) != 1 || r.Differences[0].String() != "type $[].sgv: number vs string" {
				t.Errorf("entries %+v", r)
			}
		case "socket_v3_storage/001.c":
			if r.Method != "CONNECT" || r.Path != "/socket.io/storage/connect" || r.StatusScore != 0 {
				t.Errorf("socket %+v", r)
			}
		}
	}

	results, _, err = CompareFolders(ns, other, Options{IncludeBad: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range results {
		if r.Group == "x_bad_requests" {
			found = true
			if r.StatusScore != 20 {
				t.Errorf("bad request scored %v", r.StatusScore)
			}
		}
	}
	if !found {
		t.Error("-bad should include x_bad_requests")
	}
}

func TestCompareFoldersErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)

	cases := [][2]string{
		{filepath.Join(dir, "nope"), dir},
		{dir, filepath.Join(dir, "nope")},
		{file, dir},
		{dir, file},
	}
	for _, c := range cases {
		if _, _, err := CompareFolders(c[0], c[1], Options{}); err == nil {
			t.Errorf("expected error comparing %s and %s", c[0], c[1])
		}
	}

	// empty folders are fine
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	results, missing, err := CompareFolders(a, b, Options{})
	if err != nil || len(results) != 0 || len(missing) != 0 {
		t.Errorf("empty folders %v %v %v", results, missing, err)
	}
}

func TestAggregate(t *testing.T) {
	results := []TestResult{
		{Method: "POST", Path: "/api/v1/entries", StatusScore: 100, BodyScore: 50, Score: 85, DurationDiff: 2},
		{Method: "GET", Path: "/api/v1/entries", StatusScore: 100, BodyScore: 100, Score: 100, DurationDiff: -1},
		{Method: "GET", Path: "/api/v1/entries", StatusScore: 0, BodyScore: 0, Score: 0, DurationDiff: -3},
		{Method: "ZZZ", Path: "/api/v1/entries"},
		{Method: "AAA", Path: "/api/v1/entries"},
		{Method: "WAIT", Path: "/socket.io/create"},
		{Method: "EMIT", Path: "/socket.io/create"},
		{Method: "DELETE", Path: "/api/v1/devicestatus"},
	}
	reports := Aggregate(results)

	order := []string{}
	for _, r := range reports {
		order = append(order, r.Method+" "+r.Path)
	}
	want := "DELETE /api/v1/devicestatus,GET /api/v1/entries,POST /api/v1/entries,AAA /api/v1/entries,ZZZ /api/v1/entries,EMIT /socket.io/create,WAIT /socket.io/create"
	if strings.Join(order, ",") != want {
		t.Errorf("order\n got  %s\n want %s", strings.Join(order, ","), want)
	}

	get := reports[1]
	if get.TestN != 2 || get.StatusScoreTotal != 100 || get.ScoreTotal != 100 || get.Duration200Diff != -4 {
		t.Errorf("GET report %+v", get)
	}
	if len(Aggregate(nil)) != 0 {
		t.Error("aggregate of nothing")
	}
}

func TestWorstTests(t *testing.T) {
	results := []TestResult{
		{Group: "g", Test: "perfect", Score: 100},
		{Group: "g", Test: "b", Score: 10},
		{Group: "g", Test: "a", Score: 10},
		{Group: "g", Test: "ok", Score: 95},
		{Group: "g", Test: "format only", Score: 100, Differences: []Difference{{Kind: "format"}}},
	}
	worst := WorstTests(results)
	names := []string{}
	for _, r := range worst {
		names = append(names, r.Test)
	}
	if strings.Join(names, ",") != "a,b,ok,format only" {
		t.Errorf("worst %v", names)
	}
}

func TestPrintReport(t *testing.T) {
	var out bytes.Buffer
	PrintReport(&out, nil, nil, Options{Worst: 15})
	if !strings.Contains(out.String(), "no results to compare") || strings.Contains(out.String(), "NaN") {
		t.Errorf("empty report:\n%s", out.String())
	}

	many := []Difference{}
	for i := 0; i < 5; i++ {
		many = append(many, Difference{Kind: "missing", Path: "$.k" + string(rune('a'+i)), Count: 1})
	}
	results := []TestResult{
		{Group: "v1", Test: "001.a", Method: "GET", Path: "/api/v1/entries", Status1: 200, Status2: 404, StatusScore: 0, BodyScore: 0, Score: 0, Differences: many},
		{Group: "v1", Test: "002.b", Method: "GET", Path: "/api/v1/entries", Status1: 200, Status2: 200, StatusScore: 100, BodyScore: 90, Score: 97},
		{Group: "v1", Test: "003.c", Method: "POST", Path: "/api/v1/entries", Status1: 200, Status2: 200, StatusScore: 100, BodyScore: 100, Score: 100},
	}
	missing := []MissingResult{
		{Name: "socket_a/001.x", Method: "WAIT", Endpoint: "/socket.io/x"},
		{Name: "socket_a/002.y", Method: "WAIT", Endpoint: "/socket.io/y"},
		{Name: "v3/001.z", Method: "GET", Endpoint: "/api/v3/z"},
	}

	out.Reset()
	PrintReport(&out, results, missing, Options{Worst: 1, MaxDiffs: 2})
	s := out.String()
	for _, want := range []string{
		"Status Score 50.00",
		"Body Score 45.00",
		"Overall Score 48.50",
		"TOTAL",
		"Total Tests 3",
		"Missing results (3, not scored):",
		"socket_a: 2",
		"v3: 1",
		"0.00  v1/001.a  GET /api/v1/entries  (200 vs 404)",
		"missing $.ka",
		"... 3 more",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "002.b") || strings.Contains(s, "missing $.kc") {
		t.Errorf("-worst 1 and -max-diffs 2 should limit the output:\n%s", s)
	}

	out.Reset()
	PrintReport(&out, results, missing, Options{Details: true, Worst: 1})
	s = out.String()
	for _, want := range []string{"Differences (2 tests):", "002.b", "missing $.ke", "socket_a/002.y  WAIT /socket.io/y"} {
		if !strings.Contains(s, want) {
			t.Errorf("details report missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "003.c") {
		t.Errorf("identical tests should not be listed:\n%s", s)
	}
}

func TestDifferenceString(t *testing.T) {
	cases := map[string]Difference{
		"status: 200 vs 404":  {Kind: "status", Detail: "200 vs 404", Count: 1},
		"missing $.a":         {Kind: "missing", Path: "$.a", Count: 1},
		"missing $[].a (x12)": {Kind: "missing", Path: "$[].a", Count: 12},
		"type $: a vs b":      {Kind: "type", Path: "$", Detail: "a vs b"},
	}
	for want, d := range cases {
		if got := d.String(); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func FuzzCompareIdenticalIs100(f *testing.F) {
	for _, s := range []string{`null`, `1`, `"x"`, `true`, `[]`, `{}`, `[{"a":[1,{"b":null}]}]`, `{"a":{"b":{"c":[[]]}}}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var v any
		if json.Unmarshal([]byte(s), &v) != nil {
			return
		}
		d := newDiffs()
		if got := BodyScore(v, v, d); got != 100 {
			t.Errorf("identical body %s scored %v", s, got)
		}
		if len(d.list()) != 0 {
			t.Errorf("identical body %s has differences %v", s, d.list())
		}
	})
}

func FuzzCompareIsBounded(f *testing.F) {
	f.Add(`{"a":1}`, `{"a":"1","b":2}`)
	f.Add(`[1,2,3]`, `[]`)
	f.Add(`"text"`, `{"x":1}`)
	f.Add(`null`, `[null]`)
	f.Add(`[{"a":[1]}]`, `[{"a":[1,2]},{"a":[]}]`)
	f.Fuzz(func(t *testing.T, a, b string) {
		var va, vb any
		if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
			return
		}
		got := BodyScore(va, vb, nil)
		if math.IsNaN(got) || got < 0 || got > 100 {
			t.Errorf("BodyScore(%s, %s) = %v out of range", a, b, got)
		}
	})
}
