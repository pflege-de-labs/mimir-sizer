package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type promClient struct {
	base, tenant, token, user, pass string
	http                            *http.Client
}

type sample struct {
	Labels map[string]string
	V      float64
}

// query runs an instant query; an empty result is not an error (metric simply absent).
func (c *promClient) query(ctx context.Context, q string) ([]sample, error) {
	u := strings.TrimRight(c.base, "/") + "/api/v1/query?" + url.Values{"query": {q}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.tenant != "" {
		req.Header.Set("X-Scope-OrgID", c.tenant)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return parseVector(body, resp.StatusCode)
}

func parseVector(body []byte, status int) ([]sample, error) {
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("HTTP %d: %.200s", status, body)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("HTTP %d: %s", status, r.Error)
	}
	if r.Data.ResultType != "vector" {
		return nil, fmt.Errorf("unexpected result type %q", r.Data.ResultType)
	}
	out := make([]sample, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		str, _ := s.Value[1].(string)
		v, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return nil, fmt.Errorf("bad sample value %v", s.Value[1])
		}
		out = append(out, sample{s.Metric, v})
	}
	return out, nil
}

func newPromClient(base, tenant, token, user, pass string) *promClient {
	return &promClient{base, tenant, token, user, pass, &http.Client{Timeout: 2 * time.Minute}}
}
