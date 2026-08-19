package main

import (
	"fmt"
	"log"
	"net/http"
	"nsc/req"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run main.go <name>")
		os.Exit(1)
	}

	name := os.Args[1]

	files := []string{
		// v1
		"requests/v1_activity.http",
		"requests/v1_devicestatus.http",
		"requests/v1_entries.http",
		"requests/v1_food.http",
		"requests/v1_profile.http",
		"requests/v1_treatments.http",
		"requests/v1_other.http",

		// v3
		"requests/v3_devicestatus.http",
		"requests/v3_entries.http",
		"requests/v3_food.http",
		"requests/v3_profile.http",
		"requests/v3_treatments.http",
		"requests/v3_settings.http",
		"requests/v3_other.http",

		// v2 (end to get all created things)
		"requests/v2_properties.http",
		"requests/v2_summary.http",

		"requests/x_bad_requests.http",
	}

	// Add requests/<name>.http to the front of the list if it exists
	_, err := os.Stat("requests/" + name + ".http")
	if err != nil {
		log.Fatalf("require a file to exist as setup at requests/%s.http", name)
	}

	files = append([]string{"requests/" + name + ".http"}, files...)

	for _, f := range files {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "file does not exist: %s\n", f)
			os.Exit(1)
		}
	}

	ctx := map[string]any{}

	requests := make([]*req.RequestFile, 0, len(files))

	for _, file := range files {
		rf, err := req.ParseRequestFile(file, ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error parsing request file: %v\n error %v with context: %+v\n", file, err, ctx)
			os.Exit(1)
		}

		requests = append(requests, rf)
	}

	err = req.ValidateRequests(requests, ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error validating request files: %v\n", err)
		os.Exit(1)
	}

	// Delete old requests
	if err := os.RemoveAll("./results/" + name); err != nil {
		log.Fatal(err)
	}

	for _, rf := range requests {
		// We keep adding to ctx, so we can reference things like previous auth values...
		rf.CTX = ctx
		ctx, err = ExecuteReqesutsFile(rf, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error fatal executing request file: %v\n with context: %+v\n", err, ctx)
			os.Exit(1)
		}
	}
	fmt.Println("done")
}

func ExecuteReqesutsFile(rf *req.RequestFile, name string) (newctx map[string]any, err error) {

	client := &http.Client{}

	if err := rf.Execute(client); err != nil {
		fmt.Fprintf(os.Stderr, "error executing request file: %v\n", err)
	}

	if err := rf.WriteResponsesToFolder(fmt.Sprintf("%s", name)); err != nil {
		return nil, err
	}

	return rf.CTX, nil
}

/////
// FLAG PARSING UTILS
//////

// kvmap just parses flags so something like jwt.token is the correct value in context
type kvMap map[string]any

func (k *kvMap) String() string {
	return fmt.Sprint(map[string]any(*k))
}

func (k *kvMap) Set(v string) error {
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid -v format, expected key=value got %q", v)
	}
	// error on  _ prefix
	if strings.HasPrefix(parts[0], "_") {
		return fmt.Errorf("invalid -v format, keys cannot start with _: %q", parts[0])
	}

	if *k == nil {
		*k = make(map[string]any)
	}

	path := strings.Split(parts[0], ".")
	value := parts[1]

	current := map[string]any(*k)

	for _, segment := range path[:len(path)-1] {
		next, ok := current[segment]
		if !ok {
			m := make(map[string]any)
			current[segment] = m
			current = m
			continue
		}

		m, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("key %q is already a value, cannot create nested key", segment)
		}

		current = m
	}

	current[path[len(path)-1]] = value
	return nil
}
