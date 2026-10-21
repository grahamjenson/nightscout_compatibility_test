package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Compares the results of two services, e.g.
//
//	go run diff/diff.go results/nightscout results/nighttroop
//
// The first folder is the reference (nightscout). Every result in it is
// compared to the result with the same group and name in the second folder.

type Options struct {
	IncludeBad bool // also compare x_bad_requests
	Details    bool // print the differences of every test that is not identical
	Worst      int  // number of worst tests to print with their differences
	MaxDiffs   int  // differences printed per test, 0 is unlimited
}

// Result is one saved request/response (HTTP or socket.io) from main.go
type Result struct {
	Method      string
	URL         string
	Endpoint    string // METHOD grouping path, e.g. /api/v1/entries
	Status      int
	DurationMs  int
	Body        any
	ContentType string
}

// Difference is one way the second service's result differs from the reference
type Difference struct {
	Kind   string // status, type, missing, extra, length, value, null, content-type, format
	Path   string // JSON path with array indexes generalised, e.g. $[].sgv
	Detail string
	Count  int
}

func (d Difference) String() string {
	s := d.Kind
	if d.Path != "" {
		s += " " + d.Path
	}
	if d.Detail != "" {
		s += ": " + d.Detail
	}
	if d.Count > 1 {
		s += fmt.Sprintf(" (x%d)", d.Count)
	}
	return s
}

type TestResult struct {
	Group  string
	Test   string
	Method string
	Path   string

	Status1 int
	Status2 int

	StatusScore float64
	BodyScore   float64
	Score       float64

	// Duration of the second minus the first, only when both succeeded
	DurationDiff int
	HasDuration  bool

	Differences []Difference
}

func (t TestResult) Name() string {
	return t.Group + "/" + t.Test
}

type EndpointReport struct {
	Method string
	Path   string

	StatusScoreTotal float64
	BodyScoreTotal   float64
	ScoreTotal       float64
	Duration200Diff  int
	TestN            int
}

// MissingResult is a reference result the second service has no result for
type MissingResult struct {
	Name     string
	Method   string
	Endpoint string
}

const (
	statusWeight = 0.7
	bodyWeight   = 0.3

	// Score for two values of the same type that differ, ids and dates are rarely the same
	valueMismatchScore = 90
	// Score for booleans that differ, e.g. success true vs false
	boolMismatchScore = 50
	// Score when the reference is null/empty and the other has data
	extraDataScore = 50
	// Each key only the second service returns costs this much of a key the reference returns
	extraKeyWeight = 0.25
)

func main() {
	opts := Options{}
	flag.BoolVar(&opts.IncludeBad, "bad", false, "also compare the x_bad_requests results")
	flag.BoolVar(&opts.Details, "details", false, "print the differences of every test that is not identical")
	flag.IntVar(&opts.Worst, "worst", 15, "number of worst scoring tests to print with their differences")
	flag.IntVar(&opts.MaxDiffs, "max-diffs", 10, "differences to print per test, 0 is unlimited")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run diff/diff.go [flags] results/nightscout results/<other service>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}

	results, missing, err := CompareFolders(flag.Arg(0), flag.Arg(1), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	PrintReport(os.Stdout, results, missing, opts)
}

// CompareFolders compares every result in folder1 with the result at the same
// relative path in folder2
func CompareFolders(folder1, folder2 string, opts Options) ([]TestResult, []MissingResult, error) {
	for _, f := range []string{folder1, folder2} {
		info, err := os.Stat(f)
		if err != nil {
			return nil, nil, fmt.Errorf("folder %s: %w", f, err)
		}
		if !info.IsDir() {
			return nil, nil, fmt.Errorf("%s is not a folder", f)
		}
	}

	// The setup file is named after the service (requests/<name>.http) and is
	// expected to be different for every service
	setupGroup := filepath.Base(filepath.Clean(folder1))

	results := []TestResult{}
	missing := []MissingResult{}

	err := filepath.WalkDir(folder1, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}

		rel, err := filepath.Rel(folder1, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "skipping file with unexpected path:", path)
			return nil
		}
		group, test := parts[0], strings.TrimSuffix(parts[1], ".json")

		if group == setupGroup {
			return nil
		}
		if !opts.IncludeBad && strings.Contains(group, "x_bad_requests") {
			return nil
		}

		r1, err := LoadResult(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skipping", path, ":", err)
			return nil
		}
		// SET only assigns a context value, it is not part of any API
		if r1.Method == "SET" || r1.Method == "" || r1.Endpoint == "" {
			return nil
		}

		other := filepath.Join(folder2, rel)
		r2, err := LoadResult(other)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "unreadable result", other, ":", err)
			}
			missing = append(missing, MissingResult{Name: group + "/" + test, Method: r1.Method, Endpoint: r1.Endpoint})
			return nil
		}

		tr := CompareResults(r1, r2)
		tr.Group = group
		tr.Test = test
		results = append(results, tr)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return results, missing, nil
}

// LoadResult reads a result file written by main.go
func LoadResult(file string) (Result, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Result{}, err
	}

	var saved struct {
		Request *struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		} `json:"request"`
		StatusCode *float64            `json:"statusCode"`
		DurationMs *float64            `json:"durationMs"`
		Headers    map[string][]string `json:"headers"`
		Body       any                 `json:"body"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		return Result{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if saved.Request == nil {
		return Result{}, fmt.Errorf("missing 'request'")
	}
	if saved.StatusCode == nil {
		return Result{}, fmt.Errorf("missing 'statusCode'")
	}

	r := Result{
		Method:   saved.Request.Method,
		URL:      saved.Request.URL,
		Endpoint: EndpointPath(saved.Request.URL),
		Status:   int(*saved.StatusCode),
		Body:     saved.Body,
	}
	if saved.DurationMs != nil {
		r.DurationMs = int(*saved.DurationMs)
	}
	for k, v := range saved.Headers {
		if strings.EqualFold(k, "Content-Type") && len(v) > 0 {
			r.ContentType = v[0]
		}
	}
	return r, nil
}

// EndpointPath groups URLs by the first three path segments without .json,
// e.g. http://x/api/v3/entries/abc -> /api/v3/entries
func EndpointPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return ""
	}
	clean := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".json")
	parts := strings.Split(clean, "/")
	if len(parts) > 4 {
		parts = parts[:4]
	}
	path := strings.Join(parts, "/")
	if path == "" {
		return "/"
	}
	return path
}

// CompareResults scores how compatible r2 is with the reference r1
func CompareResults(r1, r2 Result) TestResult {
	tr := TestResult{
		Method:  r1.Method,
		Path:    r1.Endpoint,
		Status1: r1.Status,
		Status2: r2.Status,
	}

	d := newDiffs()

	tr.StatusScore = StatusScore(r1.Status, r2.Status)
	if r1.Status != r2.Status {
		d.add("status", "", fmt.Sprintf("%d vs %d", r1.Status, r2.Status))
	}

	if ct1, ct2 := mediaType(r1.ContentType), mediaType(r2.ContentType); ct1 != "" && ct2 != "" && ct1 != ct2 {
		d.add("content-type", "", ct1+" vs "+ct2)
	}

	tr.BodyScore = BodyScore(r1.Body, r2.Body, d)
	tr.Score = statusWeight*tr.StatusScore + bodyWeight*tr.BodyScore

	if isSuccess(r1.Status) && isSuccess(r2.Status) {
		tr.DurationDiff = r2.DurationMs - r1.DurationMs
		tr.HasDuration = true
	}

	tr.Differences = d.list()
	return tr
}

func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	}
	return mt
}

func isSuccess(status int) bool {
	return status >= 200 && status < 300
}

// StatusScore: identical 100, same class (e.g. 400 vs 404) 80, the other
// service succeeding where the reference failed 20, failing where the
// reference succeeded (or any other mismatch) 0
func StatusScore(a, b int) float64 {
	if a == b {
		return 100
	}

	classA, classB := a/100, b/100
	if classA == classB {
		return 80
	}
	if classB == 2 && classA >= 1 && classA <= 5 {
		return 20
	}
	return 0
}

// BodyScore compares the body of the other service to the reference body. If the
// reference body is not JSON (a plain string) but the other is, that is fine.
func BodyScore(a, b any, d *diffs) float64 {
	if d == nil {
		d = newDiffs()
	}

	if s, ok := a.(string); ok {
		switch b.(type) {
		case map[string]any, []any:
			d.add("format", "$", fmt.Sprintf("text %s vs JSON %s", truncate(s, 40), typeName(b)))
			return 100
		}
	}
	return compare(a, b, "$", d)
}

// compare scores b against the reference a from 0 to 100. Types matter most,
// then missing keys and array lengths, values matter least.
func compare(a, b any, path string, d *diffs) float64 {
	if a == nil && b == nil {
		return 100
	}
	if a == nil {
		d.add("null", path, "null vs "+typeName(b))
		return extraDataScore
	}
	if b == nil {
		d.add("null", path, typeName(a)+" vs null")
		return 0
	}

	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			d.add("type", path, "object vs "+typeName(b))
			return 0
		}
		return compareObjects(av, bv, path, d)

	case []any:
		bv, ok := b.([]any)
		if !ok {
			d.add("type", path, "array vs "+typeName(b))
			return 0
		}
		return compareArrays(av, bv, path, d)

	case bool:
		bv, ok := b.(bool)
		if !ok {
			d.add("type", path, "boolean vs "+typeName(b))
			return 0
		}
		if av != bv {
			d.add("value", path, fmt.Sprintf("%v vs %v", av, bv))
			return boolMismatchScore
		}
		return 100

	case string:
		bv, ok := b.(string)
		if !ok {
			d.add("type", path, "string vs "+typeName(b))
			return 0
		}
		if av != bv {
			return valueMismatchScore
		}
		return 100

	case float64:
		bv, ok := b.(float64)
		if !ok {
			d.add("type", path, "number vs "+typeName(b))
			return 0
		}
		if av != bv {
			return valueMismatchScore
		}
		return 100

	default:
		if typeName(a) != typeName(b) {
			d.add("type", path, typeName(a)+" vs "+typeName(b))
			return 0
		}
		return 100
	}
}

func compareObjects(a, b map[string]any, path string, d *diffs) float64 {
	total := 0.0
	for _, k := range sortedKeys(a) {
		bv, ok := b[k]
		if !ok {
			d.add("missing", joinKey(path, k), "")
			continue
		}
		total += compare(a[k], bv, joinKey(path, k), d)
	}

	extra := 0
	for _, k := range sortedKeys(b) {
		if _, ok := a[k]; !ok {
			extra++
			d.add("extra", joinKey(path, k), "")
		}
	}

	if len(a) == 0 {
		if len(b) == 0 {
			return 100
		}
		return extraDataScore
	}

	return total / (float64(len(a)) + extraKeyWeight*float64(extra))
}

func compareArrays(a, b []any, path string, d *diffs) float64 {
	if len(a) != len(b) {
		d.add("length", path, fmt.Sprintf("%d vs %d", len(a), len(b)))
	}

	switch {
	case len(a) == 0 && len(b) == 0:
		return 100
	case len(a) == 0:
		return extraDataScore
	case len(b) == 0:
		return 0
	}

	n := min(len(a), len(b))
	total := 0.0
	for i := 0; i < n; i++ {
		total += compare(a[i], b[i], path+"[]", d)
	}

	lengthFactor := 0.5 + 0.5*float64(n)/float64(max(len(a), len(b)))
	return total / float64(n) * lengthFactor
}

func joinKey(path, key string) string {
	return path + "." + key
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func truncate(s string, n int) string {
	s = strconv.Quote(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + `..."`
}

// diffs collects differences, merging the same kind at the same path
// (array indexes are generalised so one difference in every element is reported once)
type diffs struct {
	order []string
	byKey map[string]*Difference
}

func newDiffs() *diffs {
	return &diffs{byKey: map[string]*Difference{}}
}

func (d *diffs) add(kind, path, detail string) {
	key := kind + " " + path
	if existing, ok := d.byKey[key]; ok {
		existing.Count++
		return
	}
	d.byKey[key] = &Difference{Kind: kind, Path: path, Detail: detail, Count: 1}
	d.order = append(d.order, key)
}

func (d *diffs) list() []Difference {
	out := make([]Difference, 0, len(d.order))
	for _, k := range d.order {
		out = append(out, *d.byKey[k])
	}
	return out
}

var methodOrder = map[string]int{
	"GET":    1,
	"POST":   2,
	"PUT":    3,
	"PATCH":  4,
	"DELETE": 5,
	// socket.io
	"CONNECT":    6,
	"EMIT":       7,
	"WAIT":       8,
	"COLLECT":    9,
	"DISCONNECT": 10,
}

func compareMethods(a, b string) int {
	oa, ok := methodOrder[a]
	if !ok {
		oa = len(methodOrder) + 1
	}
	ob, ok := methodOrder[b]
	if !ok {
		ob = len(methodOrder) + 1
	}
	if oa != ob {
		return oa - ob
	}
	return strings.Compare(a, b)
}

// Aggregate groups test results by METHOD and endpoint path
func Aggregate(results []TestResult) []*EndpointReport {
	byKey := map[string]*EndpointReport{}
	for _, r := range results {
		key := r.Method + " " + r.Path
		er, ok := byKey[key]
		if !ok {
			er = &EndpointReport{Method: r.Method, Path: r.Path}
			byKey[key] = er
		}
		er.StatusScoreTotal += r.StatusScore
		er.BodyScoreTotal += r.BodyScore
		er.ScoreTotal += r.Score
		er.Duration200Diff += r.DurationDiff
		er.TestN++
	}

	reports := make([]*EndpointReport, 0, len(byKey))
	for _, er := range byKey {
		reports = append(reports, er)
	}
	slices.SortFunc(reports, func(a, b *EndpointReport) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return compareMethods(a.Method, b.Method)
	})
	return reports
}

// WorstTests returns the tests that are not identical, lowest score first
func WorstTests(results []TestResult) []TestResult {
	worst := []TestResult{}
	for _, r := range results {
		if r.Score < 100 || len(r.Differences) > 0 {
			worst = append(worst, r)
		}
	}
	slices.SortStableFunc(worst, func(a, b TestResult) int {
		switch {
		case a.Score < b.Score:
			return -1
		case a.Score > b.Score:
			return 1
		}
		return strings.Compare(a.Name(), b.Name())
	})
	return worst
}

func PrintReport(out io.Writer, results []TestResult, missing []MissingResult, opts Options) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)

	reports := Aggregate(results)
	total := EndpointReport{}
	for _, r := range reports {
		n := float64(r.TestN)
		fmt.Fprintf(w, "%s\t%s\tStatus Score %.2f\tBody Score %.2f\tOverall Score %.2f\tDuration200Diff %d\tTotal Tests %d\n",
			r.Method, r.Path, r.StatusScoreTotal/n, r.BodyScoreTotal/n, r.ScoreTotal/n, r.Duration200Diff, r.TestN)
		total.StatusScoreTotal += r.StatusScoreTotal
		total.BodyScoreTotal += r.BodyScoreTotal
		total.ScoreTotal += r.ScoreTotal
		total.Duration200Diff += r.Duration200Diff
		total.TestN += r.TestN
	}

	if total.TestN == 0 {
		fmt.Fprintln(w, "TOTAL\t\tno results to compare")
	} else {
		n := float64(total.TestN)
		fmt.Fprintf(w, "TOTAL\t\tStatus Score %.2f\tBody Score %.2f\tOverall Score %.2f\tDuration200Diff %d\tTotal Tests %d\n",
			total.StatusScoreTotal/n, total.BodyScoreTotal/n, total.ScoreTotal/n, total.Duration200Diff, total.TestN)
	}
	w.Flush()

	if len(missing) > 0 {
		fmt.Fprintf(out, "\nMissing results (%d, not scored):\n", len(missing))
		if opts.Details {
			for _, m := range missing {
				fmt.Fprintf(out, "  %s  %s %s\n", m.Name, m.Method, m.Endpoint)
			}
		} else {
			groups := []string{}
			counts := map[string]int{}
			for _, m := range missing {
				group, _, _ := strings.Cut(m.Name, "/")
				if counts[group] == 0 {
					groups = append(groups, group)
				}
				counts[group]++
			}
			for _, g := range groups {
				fmt.Fprintf(out, "  %s: %d\n", g, counts[g])
			}
		}
	}

	worst := WorstTests(results)
	if !opts.Details && opts.Worst >= 0 && len(worst) > opts.Worst {
		worst = worst[:opts.Worst]
	}
	if len(worst) == 0 {
		return
	}

	if opts.Details {
		fmt.Fprintf(out, "\nDifferences (%d tests):\n", len(worst))
	} else {
		fmt.Fprintf(out, "\nWorst tests (lowest score first, -details for all):\n")
	}
	for _, t := range worst {
		fmt.Fprintf(out, "  %.2f  %s  %s %s  (%d vs %d)\n", t.Score, t.Name(), t.Method, t.Path, t.Status1, t.Status2)
		for i, diff := range t.Differences {
			if opts.MaxDiffs > 0 && i >= opts.MaxDiffs {
				fmt.Fprintf(out, "        ... %d more\n", len(t.Differences)-i)
				break
			}
			fmt.Fprintf(out, "        %s\n", diff)
		}
	}
}
