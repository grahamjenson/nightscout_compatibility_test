package sio

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"nsc/req"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Socket steps have no HTTP status, so results use these to make them comparable with diff
const (
	StatusOK      = 200
	StatusTimeout = 408
	StatusError   = 500
)

type Result struct {
	Step       Step // rendered
	Args       []any
	StatusCode int
	Body       any
	Error      string
	Duration   time.Duration

	// Set when the step is an HTTP DSL request
	HTTP *req.Response
}

type savedResult struct {
	Request    savedSocketRequest `json:"request"`
	StatusCode int                `json:"statusCode"`
	Body       any                `json:"body"`
	Error      string             `json:"error,omitempty"`
	DurationMs int64              `json:"durationMs"`
}

type savedSocketRequest struct {
	Method string `json:"method"`
	// Path and URL are /socket.io/<namespace>/<event> so diff groups results by namespace and event
	Path       string      `json:"path"`
	URL        string      `json:"url"`
	ConnectURL string      `json:"connectUrl"`
	Namespace  string      `json:"namespace"`
	Event      string      `json:"event,omitempty"`
	Connection string      `json:"connection"`
	Ack        bool        `json:"ack,omitempty"`
	Headers    http.Header `json:"headers,omitempty"`
	Args       []any       `json:"args,omitempty"`
}

// Execute runs every step in order. Socket failures are recorded as results
// rather than stopping the script, so they show up when diffing services.
func (sf *ScriptFile) Execute(httpClient *http.Client) error {
	sf.Results = []Result{}

	conns := map[string]*Client{}
	lastConn := ""
	defer func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	}()

	for i, step := range sf.Steps {
		var res Result

		if step.HTTP != nil {
			httpReq, err := step.HTTP.BuildRealRequest(sf.CTX)
			if err != nil {
				return fmt.Errorf("error rendering request %q: %w", step.Name, err)
			}
			resp, err := httpReq.Execute(httpClient)
			if err != nil {
				return fmt.Errorf("error executing request %q: %w", step.Name, err)
			}
			req.SetContext(sf.CTX, step.Name, resp.Body)
			res = Result{Step: step, StatusCode: resp.StatusCode, HTTP: &resp}
		} else {
			rendered, args, err := step.render(sf.CTX)
			if err != nil {
				return err
			}

			connName := rendered.Conn
			if connName == "" {
				connName = lastConn
			}
			if rendered.Verb == VerbConnect {
				connName = rendered.Name
				lastConn = rendered.Name
			}
			rendered.Conn = connName

			res = sf.executeSocketStep(rendered, args, conns)
			sf.CTX[step.Name] = res.Body
		}

		sf.Results = append(sf.Results, res)
		suffix := ""
		if res.Error != "" {
			suffix = " (" + res.Error + ")"
		}
		fmt.Printf("[%d/%d] %s -> %d%s\n", i+1, len(sf.Steps), step.Name, res.StatusCode, suffix)
	}

	return nil
}

func (sf *ScriptFile) executeSocketStep(step Step, args []any, conns map[string]*Client) Result {
	res := Result{Step: step, Args: args}
	step.print()

	start := time.Now()

	if step.Verb == VerbConnect {
		var auth any
		if len(args) > 0 {
			auth = args[0]
		}
		c, data, err := Dial(step.Target, step.Headers, auth, step.Timeout)
		conns[step.Name] = c
		res.Body = data
		res.Duration = time.Since(start)
		return res.finish(err)
	}

	c := conns[step.Conn]
	if c == nil {
		res.Duration = time.Since(start)
		return res.finish(fmt.Errorf("connection %q is not open", step.Conn))
	}

	var err error
	switch step.Verb {
	case VerbEmit:
		var ackArgs []any
		ackArgs, err = c.Emit(step.Target, args, step.Ack, step.Timeout)
		res.Body = unwrapArgs(ackArgs)

	case VerbWait:
		var e Event
		e, err = c.Wait(step.Target, step.Timeout)
		res.Body = unwrapArgs(e.Args)

	case VerbCollect:
		d, _ := time.ParseDuration(step.Target)
		events := []any{}
		for _, e := range c.Collect(d) {
			events = append(events, map[string]any{"event": e.Name, "args": e.Args})
		}
		res.Body = events

	case VerbDisconnect:
		c.Close()
		delete(conns, step.Conn)
	}

	res.Duration = time.Since(start)
	return res.finish(err)
}

func (r Result) finish(err error) Result {
	switch {
	case err == nil:
		r.StatusCode = StatusOK
	case errors.Is(err, ErrTimeout):
		r.StatusCode = StatusTimeout
		r.Error = err.Error()
	default:
		r.StatusCode = StatusError
		r.Error = err.Error()
	}
	return r
}

// unwrapArgs makes a single argument the body so it can be referenced with dig, e.g. {{ dig "subscribe.success" }}
func unwrapArgs(args []any) any {
	switch len(args) {
	case 0:
		return nil
	case 1:
		return args[0]
	default:
		return args
	}
}

// render fills in templates and parses the B lines as JSON
func (s Step) render(ctx map[string]any) (Step, []any, error) {
	target, err := req.RenderTemplate(s.Target, ctx)
	if err != nil {
		return Step{}, nil, fmt.Errorf("step %s render failed: %w", s.Name, err)
	}
	s.Target = target

	headers := make(http.Header)
	for k, vv := range s.Headers {
		for _, v := range vv {
			rendered, err := req.RenderTemplate(v, ctx)
			if err != nil {
				return Step{}, nil, fmt.Errorf("step %s header %s render failed: %w", s.Name, k, err)
			}
			headers.Add(k, rendered)
		}
	}
	s.Headers = headers

	renderedArgs := make([]string, len(s.Args))
	args := make([]any, len(s.Args))
	for i, a := range s.Args {
		rendered, err := req.RenderTemplate(a, ctx)
		if err != nil {
			return Step{}, nil, fmt.Errorf("step %s body render failed: %w", s.Name, err)
		}
		renderedArgs[i] = rendered
		if err := json.Unmarshal([]byte(rendered), &args[i]); err != nil {
			return Step{}, nil, fmt.Errorf("step %s body %d is not JSON: %w", s.Name, i+1, err)
		}
	}
	s.Args = renderedArgs

	return s, args, nil
}

func (s Step) print() {
	fmt.Printf("NAME %s\n", s.Name)
	fmt.Printf("%s %s\n", s.Verb, s.Target)
	if s.Verb != VerbConnect {
		fmt.Printf("C %s\n", s.Conn)
	}
	for k, vv := range s.Headers {
		for _, v := range vv {
			fmt.Printf("H %s: %s\n", k, v)
		}
	}
	for _, a := range s.Args {
		fmt.Printf("B %s\n", a)
	}
	if s.Ack {
		fmt.Println("ACK")
	}
	fmt.Println()
}

func (sf *ScriptFile) WriteResultsToFolder(name string) error {
	folder := fmt.Sprintf("results/%s/%s", name, sf.Name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}

	// CONNECT URLs by step name, so later steps can record where they were sent
	connectURLs := map[string]string{}

	for i, res := range sf.Results {
		filename := filepath.Join(
			folder,
			fmt.Sprintf("%03d.%s.json", i+1, req.SanitizeFilename(res.Step.Name)),
		)

		if res.HTTP != nil {
			if err := req.SaveResponse(filename, *res.HTTP); err != nil {
				return err
			}
			continue
		}

		if res.Step.Verb == VerbConnect {
			connectURLs[res.Step.Name] = res.Step.Target
		}

		out := savedResult{
			Request:    savedRequestFor(res, connectURLs[res.Step.Conn]),
			StatusCode: res.StatusCode,
			Body:       res.Body,
			Error:      res.Error,
			DurationMs: res.Duration.Milliseconds(),
		}
		if err := req.WriteJSONFile(filename, out); err != nil {
			return err
		}
	}

	return nil
}

func savedRequestFor(res Result, connectURL string) savedSocketRequest {
	s := res.Step
	event := s.Target
	switch s.Verb {
	case VerbConnect:
		event = "connect"
	case VerbDisconnect:
		event = "disconnect"
	case VerbCollect:
		event = "events"
	}

	out := savedSocketRequest{
		Method:     s.Verb,
		ConnectURL: connectURL,
		Event:      event,
		Connection: s.Conn,
		Ack:        s.Ack,
		Headers:    s.Headers,
		Args:       res.Args,
	}
	if len(out.Headers) == 0 {
		out.Headers = nil
	}

	u, err := url.Parse(connectURL)
	if err != nil {
		return out
	}
	out.Namespace = strings.TrimSuffix(u.Path, "/")
	if out.Namespace == "" {
		out.Namespace = "/"
	}

	out.Path = "/socket.io" + strings.TrimSuffix(out.Namespace, "/") + "/" + event
	out.URL = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: out.Path}).String()
	return out
}
