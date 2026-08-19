package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"text/tabwriter"
)

type CompareFile struct {
	ServiceName1 string // should be nightscout
	ServiceName2 string // should be other service name
	GroupName    string
	TestName     string
	Method       string // GET, POST, etc.
	Path         string // /api/v1/entries.json, etc.
}

func (c CompareFile) Name() string {
	return fmt.Sprintf("%s.%s", c.GroupName, c.TestName)
}
func (c CompareFile) FilePaths() (string, string) {
	return filepath.Join(PREFIX, c.ServiceName1, c.GroupName, fmt.Sprintf("%s.json", c.TestName)), filepath.Join(PREFIX, c.ServiceName2, c.GroupName, fmt.Sprintf("%s.json", c.TestName))
}

func (c CompareFile) MethodPath() string {
	return fmt.Sprintf("%s %s", c.Method, c.Path)
}

type EndpointCompatibility struct {
	Method string
	Path   string

	StatusScoreTotal float64 // 0-100
	BodyScoreTotal   float64 // 0-100
	Duration200Diff  int
	TestN            int     // number of tests compared
	ScoreTotal       float64 // weighted overall
}

var PREFIX = "results"

func getNameFromFolder(path string) string {
	return strings.TrimPrefix(path, PREFIX)
}

func main() {
	// the two args are folder 1 and folder 2
	folder1 := os.Args[1]
	folder2 := os.Args[2]

	// check folder 1 and folder 2 exist
	if _, err := os.Stat(folder1); os.IsNotExist(err) {
		fmt.Println("Folder 1 does not exist", folder1)
		return
	}
	if _, err := os.Stat(folder2); os.IsNotExist(err) {
		fmt.Println("Folder 2 does not exist", folder2)
		return
	}
	name1 := getNameFromFolder(folder1)
	name2 := getNameFromFolder(folder2)
	compareFiles := getCompareFiles(folder1, name1, name2)

	fmt.Println("Comparing folders...")
	reports := map[string]*EndpointCompatibility{}

	for _, file := range compareFiles {
		statusCompatibility, bodyCompatibility, duration200diff, err := generateDiffReport(file)
		if err != nil {
			fmt.Println("Error generating diff report for file", file.Name(), ":", err)
			continue
		}
		if reports[file.MethodPath()] == nil {
			reports[file.MethodPath()] = &EndpointCompatibility{
				Method:           file.Method,
				Path:             file.Path,
				StatusScoreTotal: statusCompatibility,
				BodyScoreTotal:   bodyCompatibility,
				ScoreTotal:       (statusCompatibility*0.7 + bodyCompatibility*0.3),
				Duration200Diff:  duration200diff,
				TestN:            1,
			}
		} else {
			ec := reports[file.MethodPath()]
			ec.StatusScoreTotal += statusCompatibility
			ec.BodyScoreTotal += bodyCompatibility
			ec.ScoreTotal += (statusCompatibility*0.7 + bodyCompatibility*0.3)
			ec.Duration200Diff += duration200diff
			ec.TestN++
		}

	}

	reportsList := []*EndpointCompatibility{}
	for _, report := range reports {
		reportsList = append(reportsList, report)
	}

	// sort reports by overall score
	slices.SortFunc(reportsList, func(a, b *EndpointCompatibility) int {
		// 1. Compare the primary field (Path)
		if a.Path != b.Path {
			if a.Path < b.Path {
				return -1
			}
			return 1
		}

		// 2. Fallback to secondary field (Method) if Paths match
		if a.Method != b.Method {
			// sort GET, POST, PUT, PATCH, DELETE
			order := map[string]int{
				"GET":    1,
				"POST":   2,
				"PUT":    3,
				"PATCH":  4,
				"DELETE": 5,
			}
			if order[a.Method] < order[b.Method] {
				return -1
			}
			return 1
		}

		return 0
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	totalTests := 0
	totalStatusScore := 0.0
	totalBodyScore := 0.0
	totalDurationDiff := 0
	for _, report := range reportsList {
		statusScore := report.StatusScoreTotal / float64(report.TestN)
		bodyScore := report.BodyScoreTotal / float64(report.TestN)
		overallScore := report.ScoreTotal / float64(report.TestN)
		totalTests += report.TestN
		totalStatusScore += report.StatusScoreTotal
		totalBodyScore += report.BodyScoreTotal
		totalDurationDiff += report.Duration200Diff
		fmt.Fprintf(w, "%s\t%s\tStatus Score %.2f\tBody Score %.2f\tOverall Score %.2f\tDuration200Diff %d\tTotal Tests %d\n", report.Method, report.Path, statusScore, bodyScore, overallScore, report.Duration200Diff, report.TestN)
	}

	fmt.Fprintf(w, "TOTAL\t\tStatus Score %.2f\tBody Score %.2f\tOverall Score %.2f\tDuration200Diff %d\tTotal Tests %d\n", totalStatusScore/float64(totalTests), totalBodyScore/float64(totalTests), (totalStatusScore*0.7+totalBodyScore*0.3)/float64(totalTests), totalDurationDiff, totalTests)
	w.Flush()
}

func getCompareFiles(folder string, name1, name2 string) []CompareFile {
	// get all subfolders and files in the folder
	comparefiles := []CompareFile{}

	err := filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			// split path into parts
			parts := strings.Split(path, string(os.PathSeparator))
			if len(parts) < 4 {
				fmt.Println("Skipping file with unexpected path format:", path)
				return nil
			}
			// parts[0] is results, parts[1] is name1 or name 2
			groupName := parts[2]

			method, apiPath, err := getMethodAndPathFromFile(path)
			if err != nil {
				fmt.Println("Error getting method and path from file", path, ":", err)
				return nil
			}
			if method == "" || apiPath == "" {
				fmt.Println("Skipping file with missing method or path:", path)
				return nil
			}
			testName := strings.TrimSuffix(parts[3], ".json")
			comparefiles = append(comparefiles, CompareFile{
				ServiceName1: name1,
				ServiceName2: name2,
				GroupName:    groupName,
				TestName:     testName,
				Method:       method,
				Path:         apiPath,
			})
		}
		return nil
	})
	if err != nil {
		fmt.Println("Error walking the path", folder, ":", err)
	}
	fmt.Println("Found", len(comparefiles), "files to compare", folder, "with", name1, "and", name2)
	// only keep compare file if both files exist in folder 1 and folder 2
	cleanCompareFiles := []CompareFile{}
	for _, compareFile := range comparefiles {
		file1, file2 := compareFile.FilePaths()
		if _, err := os.Stat(file1); os.IsNotExist(err) {
			fmt.Println("File", file1, "does not exist")
			continue
		}
		if _, err := os.Stat(file2); os.IsNotExist(err) {
			fmt.Println("File", file2, "does not exist")
			continue
		}

		if strings.Contains(compareFile.GroupName, "x_bad_requests") {
			fmt.Println("Skipping bad request file", compareFile.Name())
			continue
		}
		cleanCompareFiles = append(cleanCompareFiles, compareFile)
	}
	return cleanCompareFiles
}

func generateDiffReport(compare CompareFile) (float64, float64, int, error) {
	file1, file2 := compare.FilePaths()
	body1, status1, duration1, err := getBodyStatusTimeFromFile(file1)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("error getting body and status from file %s: %w", file1, err)
	}
	body2, status2, duration2, err := getBodyStatusTimeFromFile(file2)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("error getting body and status from file %s: %w", file2, err)
	}

	statusCompatibility := statusScore(status1, status2)
	bodyCompatibility := bodyCompatabilityScore(body1, body2)

	durationdiff := 0
	if status1 == 200 || status1 == 201 && status2 == 200 || status2 == 201 {
		durationdiff = duration2 - duration1
	}
	return statusCompatibility, bodyCompatibility, durationdiff, nil
}

// If the first fails and the second succeeds, it is not bad
// If the first succeeds and the second fails, it is bad
// 200 vs 201 is bad but not as bad as 200 vs 500
func statusScore(a, b int) float64 {
	if a == b {
		return 100
	}

	classA := a / 100
	classB := b / 100
	// Same class of error at least ball park
	if classA == classB {
		return 80
	}

	if classA == 2 && (classB == 1 || classB == 3 || classB == 4 || classB == 5) {
		// A succeeded and B did not, bad
		return 0
	}

	if classB == 2 && (classA == 1 || classA == 3 || classA == 4 || classA == 5) {
		// B succeeded and A did not, not as bad but still bad
		return 20
	}

	return 0
}

// If body 1 is not json, but body 2 is, that is fine.
// Types matter above all else, e.g. if it is an array return then must return an array
// Then number of values matter next
// Then key names of first type matter next
// Values matter the least
func bodyCompatabilityScore(body1, body2 any) float64 {
	return compatibility(body1, body2)
}

func compatibility(a, b any) float64 {
	// nil handling
	if a == nil && b == nil {
		return 100
	}
	if a == nil || b == nil {
		return 0
	}

	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return 0
		}

		if len(av) == 0 && len(bv) == 0 {
			return 100
		}

		// 30% key compatibility
		keyMatches := 0
		for k := range av {
			if _, ok := bv[k]; ok {
				keyMatches++
			}
		}

		keyScore := float64(keyMatches) / float64(max(len(av), 1))

		// 20% count compatibility
		countScore := 1.0
		if len(av) > 0 || len(bv) > 0 {
			countScore = float64(min(len(av), len(bv))) /
				float64(max(len(av), len(bv)))
		}

		// 10% value compatibility
		valueScore := 0.0
		if keyMatches > 0 {
			total := 0.0
			for k := range av {
				if bvv, ok := bv[k]; ok {
					total += compatibility(av[k], bvv) / 100
				}
			}
			valueScore = total / float64(keyMatches)
		}

		return 40 +
			(30 * keyScore) +
			(20 * countScore) +
			(10 * valueScore)

	case []any:
		bv, ok := b.([]any)
		if !ok {
			return 0
		}

		if len(av) == 0 && len(bv) == 0 {
			return 100
		}

		// 20% count compatibility
		countScore := float64(min(len(av), len(bv))) /
			float64(max(len(av), len(bv)))

		// Compare elements by position
		valueScore := 0.0
		n := min(len(av), len(bv))

		if n > 0 {
			for i := 0; i < n; i++ {
				valueScore += compatibility(av[i], bv[i]) / 100
			}
			valueScore /= float64(n)
		}

		return 40 + // type matched
			30 + // arrays matched
			(20 * countScore) +
			(10 * valueScore)

	case string:
		bv, ok := b.(string)
		if !ok {
			return 0
		}

		if av == bv {
			return 100
		}

		return 90

	case float64:
		_, ok := b.(float64)
		if !ok {
			return 0
		}

		if reflect.DeepEqual(a, b) {
			return 100
		}

		return 90

	case bool:
		bv, ok := b.(bool)
		if !ok {
			return 0
		}

		if av == bv {
			return 100
		}

		return 90

	default:
		if reflect.DeepEqual(a, b) {
			return 100
		}
		return 50
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func getMethodAndPathFromFile(file string) (string, string, error) {
	var jsonmap map[string]any
	fileRaw, err := os.ReadFile(file)
	if err != nil {
		return "", "", fmt.Errorf("error reading file: %w", err)
	}
	if err := json.Unmarshal(fileRaw, &jsonmap); err != nil {
		return "", "", fmt.Errorf("error unmarshaling JSON: %w", err)
	}

	request, ok := jsonmap["request"]
	if !ok {
		return "", "", fmt.Errorf("missing 'request' field in JSON")
	}
	requestMap, ok := request.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("'request' field is not an object")
	}

	method, ok := requestMap["method"].(string)
	if !ok {
		return "", "", fmt.Errorf("missing or invalid 'method' field in 'request'")
	}

	urlString, ok := requestMap["url"].(string)
	if !ok {
		return "", "", fmt.Errorf("missing or invalid 'url' field in 'request'")
	}

	path := urlString
	u, err := url.Parse(urlString)
	if err != nil {
		return "", "", fmt.Errorf("missing or invalid 'url' field in 'request'")
	} else {
		cleanPath := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".json")
		pathParts := strings.Split(cleanPath, "/")
		if len(pathParts) >= 4 {
			path = strings.Join(pathParts[:4], "/")
		} else {
			path = u.Path
		}
	}

	fmt.Println("Extracted method and path from file", file, ":", method, " -- ", path)
	return method, path, nil
}

func getBodyStatusTimeFromFile(file string) (any, int, int, error) {
	var jsonmap map[string]any
	fileRaw, err := os.ReadFile(file)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("error reading file: %w", err)
	}
	if err := json.Unmarshal(fileRaw, &jsonmap); err != nil {
		return nil, 0, 0, fmt.Errorf("error unmarshaling JSON: %w", err)
	}

	status, ok := jsonmap["statusCode"].(float64)
	if !ok {
		return nil, 0, 0, fmt.Errorf("missing or invalid 'statusCode' field in JSON")
	}

	duration, ok := jsonmap["durationMs"].(float64)
	if !ok {
		return nil, 0, 0, fmt.Errorf("missing or invalid 'durationMs' field in JSON")
	}
	return jsonmap["body"], int(status), int(duration), nil
}
