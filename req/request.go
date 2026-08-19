package req

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"
)

type Request struct {
	Name     string
	Method   string
	URL      string
	Headers  http.Header
	Body     string
	IsMethod bool
}

func (r Request) PrintReqeust() {
	fmt.Printf("NAME %s\n", r.Name)
	fmt.Printf("%s %s\n", r.Method, r.URL)
	for k, vv := range r.Headers {
		for _, v := range vv {
			fmt.Printf("H %s: %s\n", k, v)
		}
	}
	if len(r.Body) > 0 {
		fmt.Printf("B %s\n", string(r.Body))
	}
	fmt.Println()
}

func (r Request) executeMethod() (Response, error) {
	if !r.IsMethod {
		return Response{}, fmt.Errorf("request %q is not a method", r.Name)
	}

	if r.Method == "SET" {
		value := string(r.URL)

		return Response{
			Name: r.Name,
			Body: []byte(value),
		}, nil
	}
	return Response{}, fmt.Errorf("unsupported method %q", r.Method)
}

func (req Request) Execute(client *http.Client) (Response, error) {
	req.PrintReqeust()

	if req.IsMethod {
		return req.executeMethod()
	}

	// ---- prefix URL with scheme, server, and port ----
	httpReq, err := http.NewRequest(req.Method, req.URL, strings.NewReader(string(req.Body)))
	if err != nil {
		return Response{}, err
	}

	httpReq.Header = req.Headers.Clone()

	// ---- execute ----
	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	duration := time.Since(start)

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return Response{}, fmt.Errorf("error reading response body for request %q: %w", req.Name, err)
	}

	return Response{
		Request:    req,
		Name:       req.Name,
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       respBody,
		Duration:   duration,
	}, nil
}

func (req Request) BuildRealRequest(ctx map[string]any) (Request, error) {

	url, err := renderTemplate(req.URL, ctx)
	if err != nil {
		return Request{}, fmt.Errorf("request %s url render failed: %w", req.Name, err)
	}

	if req.IsMethod {
		return Request{
			Name:     req.Name,
			Method:   req.Method,
			URL:      url,
			Headers:  req.Headers.Clone(),
			Body:     req.Body,
			IsMethod: true,
		}, nil
	}

	// ---- render body ----
	bodyStr, err := renderTemplate(string(req.Body), ctx)
	if err != nil {
		return Request{}, fmt.Errorf("request %s body render failed: %w", req.Name, err)
	}

	// -- render headers ----
	renderedHeaders := make(http.Header)
	for k, vv := range req.Headers {
		for _, v := range vv {
			renderedValue, err := renderTemplate(v, ctx)
			if err != nil {
				return Request{}, fmt.Errorf("request %s header %s render failed: %w", req.Name, k, err)
			}
			renderedHeaders.Add(k, renderedValue)
		}
	}
	req.Headers = renderedHeaders

	// Default content and accept types
	if req.Headers.Get("Content-Type") == "" {
		req.Headers.Set("Content-Type", "application/json")
	}
	if req.Headers.Get("Accept") == "" {
		req.Headers.Set("Accept", "application/json")
	}
	return Request{
		Name:     req.Name,
		Method:   req.Method,
		URL:      url,
		Headers:  renderedHeaders,
		Body:     bodyStr,
		IsMethod: req.IsMethod,
	}, nil
}

func templateTime(offset int) string {
	// top of the minute
	t := time.Now().Add(-time.Duration(offset) * time.Minute)
	return t.Format(time.RFC3339)
}

func templateITime(offset int) int64 {
	// top of the minute
	t := time.Now().Add(-time.Duration(offset) * time.Minute)
	return t.UnixMilli()
}

func templateDig(root any, path string) any {
	parts := strings.Split(path, ".")
	var cur any = root

	for _, p := range parts {
		switch v := cur.(type) {

		case map[string]any:
			next, ok := v[p]
			if !ok {
				return nil
			}
			cur = next

		case []any:
			// allow numeric indexing into slices
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(v) {
				return nil
			}
			cur = v[i]

		default:
			return nil
		}
	}

	return cur
}

func renderTemplate(input string, ctx map[string]any) (string, error) {
	tmpl, err := template.New("req").
		Option("missingkey=error").
		Funcs(template.FuncMap{
			"dig": func(path string) any {
				return templateDig(ctx, path)
			},
			"token": func() string {
				return ctx["token"].(string)
			},
			"read_token": func() string {
				return ctx["read_token"].(string)
			},
			"jwt": func() string {
				return ctx["jwt"].(string)
			},
			"read_jwt": func() string {
				return ctx["read_jwt"].(string)
			},
			"time":  templateTime,
			"itime": templateITime,
			"tokenByName": func(list []any, name string) string {
				for _, item := range list {
					m := item.(map[string]any)
					if m["name"] == name {
						return m["accessToken"].(string)
					}
				}
				return ""
			},
		}).
		Parse(input)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", err
	}

	return buf.String(), nil
}
