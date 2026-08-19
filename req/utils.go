package req

import (
	"fmt"
	"regexp"
)

func ValidateRequests(requests []*RequestFile, ctx map[string]any) error {
	// Make sure no two have the same name
	// Make sure no request overwrites the context name
	// Make sure all requests have a method and URL

	nameSet := make(map[string]any)

	for name := range ctx {
		nameSet[name] = ""
	}
	for _, rf := range requests {
		for _, r := range rf.Requests {
			if r.Name == "" {
				return fmt.Errorf("request with empty name")
			}
			if _, exists := nameSet[r.Name]; exists {
				return fmt.Errorf("duplicate request name: %q", r.Name)
			}
			nameSet[r.Name] = ""

			if r.Method == "" {
				return fmt.Errorf("request %q missing method", r.Name)
			}
			if r.URL == "" {
				return fmt.Errorf("request %q missing URL", r.Name)
			}
		}
	}

	for name := range ctx {
		// All names must be go variable compliant
		regex := "^[a-zA-Z_][a-zA-Z0-9_]*$"
		matched, err := regexp.MatchString(regex, name)
		if err != nil {
			return fmt.Errorf("failed to validate context name %q: %w", name, err)
		}
		if !matched {
			return fmt.Errorf("invalid context name %q: must match regex %q", name, regex)
		}
	}
	return nil
}
