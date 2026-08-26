package main

import (
	"io"
	"net/http"
	"strings"
)

func httpNew(method, url, body string) (*http.Request, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func httpDo(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}
