package sio

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"nsc/req"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultTimeout = 10 * time.Second

// Socket verbs. Any other verb (GET, POST, SET, ...) is an HTTP DSL request.
const (
	VerbConnect    = "CONNECT"
	VerbEmit       = "EMIT"
	VerbWait       = "WAIT"
	VerbCollect    = "COLLECT"
	VerbDisconnect = "DISCONNECT"
)

var socketVerbs = []string{VerbConnect, VerbEmit, VerbWait, VerbCollect, VerbDisconnect}

type Step struct {
	Name string
	Verb string

	// URL for CONNECT, event name for EMIT and WAIT, duration for COLLECT
	Target string
	// Name of the CONNECT step to use, defaults to the most recent CONNECT
	Conn string
	// One JSON value per B line, the event arguments for EMIT or the auth payload for CONNECT
	Args    []string
	Headers http.Header
	Ack     bool
	Timeout time.Duration

	// Set when this step is an HTTP DSL request rather than a socket step
	HTTP *req.Request
}

type ScriptFile struct {
	Name    string
	Steps   []Step
	Results []Result
	CTX     map[string]any
}

func ParseScriptFile(file string) (*ScriptFile, error) {
	r, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	steps, err := parseScript(r)
	if err != nil {
		return nil, err
	}

	// name is the file name without folders or extension
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	return &ScriptFile{
		Name:  name,
		Steps: steps,
	}, nil
}

func parseScript(r io.Reader) ([]Step, error) {
	// Split the file into blocks that each start with a NAME line
	var blocks [][]string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)

		if strings.HasPrefix(line, "NAME ") {
			blocks = append(blocks, []string{raw})
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("line before NAME: %q", line)
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], raw)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	steps := make([]Step, 0, len(blocks))
	for _, block := range blocks {
		step, err := parseBlock(block)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func parseBlock(block []string) (Step, error) {
	name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(block[0]), "NAME "))
	if name == "" {
		return Step{}, fmt.Errorf("empty step name")
	}

	if len(block) < 2 {
		return Step{}, fmt.Errorf("step %q has no verb", name)
	}

	verb, target, _ := strings.Cut(strings.TrimSpace(block[1]), " ")
	if !isSocketVerb(verb) {
		// Hand anything else to the HTTP DSL parser
		requests, err := req.ParseRequests(strings.NewReader(strings.Join(block, "\n")))
		if err != nil {
			return Step{}, fmt.Errorf("step %q: %w", name, err)
		}
		if len(requests) != 1 {
			return Step{}, fmt.Errorf("step %q: expected one request got %d", name, len(requests))
		}
		return Step{Name: name, Verb: requests[0].Method, HTTP: &requests[0]}, nil
	}

	step := Step{
		Name:    name,
		Verb:    verb,
		Target:  strings.TrimSpace(target),
		Headers: make(http.Header),
		Timeout: DefaultTimeout,
	}

	for _, raw := range block[2:] {
		line := strings.TrimSpace(raw)

		// Continuation of the last B line
		if len(step.Args) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
			step.Args[len(step.Args)-1] += "\n" + line
			continue
		}

		switch {
		case strings.HasPrefix(line, "B "):
			step.Args = append(step.Args, strings.TrimPrefix(line, "B "))

		case strings.HasPrefix(line, "H "):
			header := strings.TrimSpace(strings.TrimPrefix(line, "H "))
			k, v, ok := strings.Cut(header, ":")
			if !ok {
				return Step{}, fmt.Errorf("step %q invalid header: %q", name, header)
			}
			step.Headers.Add(strings.TrimSpace(k), strings.TrimSpace(v))

		case strings.HasPrefix(line, "C "):
			step.Conn = strings.TrimSpace(strings.TrimPrefix(line, "C "))

		case strings.HasPrefix(line, "T "):
			d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(line, "T ")))
			if err != nil {
				return Step{}, fmt.Errorf("step %q invalid timeout: %w", name, err)
			}
			step.Timeout = d

		case line == "ACK":
			step.Ack = true

		default:
			return Step{}, fmt.Errorf("step %q unknown line: %q", name, line)
		}
	}

	return step, step.validate()
}

func (s Step) validate() error {
	switch s.Verb {
	case VerbConnect:
		if s.Target == "" {
			return fmt.Errorf("step %q: CONNECT needs a URL", s.Name)
		}
		if len(s.Args) > 1 {
			return fmt.Errorf("step %q: CONNECT takes at most one B (auth payload)", s.Name)
		}
	case VerbEmit:
		if s.Target == "" {
			return fmt.Errorf("step %q: EMIT needs an event name", s.Name)
		}
	case VerbWait:
		if s.Target == "" {
			return fmt.Errorf("step %q: WAIT needs an event name", s.Name)
		}
	case VerbCollect:
		if _, err := time.ParseDuration(s.Target); err != nil {
			return fmt.Errorf("step %q: COLLECT needs a duration: %w", s.Name, err)
		}
	}

	if s.Ack && s.Verb != VerbEmit {
		return fmt.Errorf("step %q: ACK is only valid with EMIT", s.Name)
	}
	if len(s.Args) > 0 && s.Verb != VerbEmit && s.Verb != VerbConnect {
		return fmt.Errorf("step %q: B is only valid with EMIT or CONNECT", s.Name)
	}
	if len(s.Headers) > 0 && s.Verb != VerbConnect {
		return fmt.Errorf("step %q: H is only valid with CONNECT", s.Name)
	}
	if s.Conn != "" && s.Verb == VerbConnect {
		return fmt.Errorf("step %q: C is not valid with CONNECT", s.Name)
	}
	return nil
}

func isSocketVerb(verb string) bool {
	for _, v := range socketVerbs {
		if v == verb {
			return true
		}
	}
	return false
}

// Validate checks step names are unique across scripts and do not clash with
// names already used (e.g. by HTTP request files), and that every C line
// references an earlier CONNECT step in the same script.
func Validate(scripts []*ScriptFile, usedNames map[string]bool) error {
	names := map[string]bool{}
	for n := range usedNames {
		names[n] = true
	}

	for _, sf := range scripts {
		connects := map[string]bool{}
		for _, s := range sf.Steps {
			if names[s.Name] {
				return fmt.Errorf("duplicate step name: %q", s.Name)
			}
			names[s.Name] = true

			if s.HTTP != nil {
				if s.HTTP.URL == "" {
					return fmt.Errorf("step %q missing URL", s.Name)
				}
				continue
			}

			if s.Verb == VerbConnect {
				connects[s.Name] = true
				continue
			}
			if s.Conn != "" && !connects[s.Conn] {
				return fmt.Errorf("step %q: C %q is not an earlier CONNECT step in %s", s.Name, s.Conn, sf.Name)
			}
			if s.Conn == "" && len(connects) == 0 {
				return fmt.Errorf("step %q: %s before any CONNECT in %s", s.Name, s.Verb, sf.Name)
			}
		}
	}
	return nil
}
