package liteenroll

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	wendy "github.com/wendylabsinc/wendy-go"
)

type discoveryHTTP struct {
	wendy.LiteHTTPTransport
	refusal string
}

func (h discoveryHTTP) DoLite(ctx context.Context, request wendy.HTTPRequest) (*wendy.LiteHTTPResponse, error) {
	response, err := h.LiteHTTPTransport.DoLite(ctx, request)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case 301, 302, 303, 307, 308:
		if response.Headers.Get("Location") != "" {
			response.Body.Close()
			op := "Get"
			if request.Method == http.MethodHead {
				op = "Head"
			}
			return nil, &url.Error{Op: op, URL: response.Headers.Get("Location"), Err: errors.New(h.refusal)}
		}
	}
	return response, nil
}
func newDiscoveryHTTP(client *http.Client, refusal string) (wendy.LiteHTTPTransport, error) {
	host, err := wendy.NewLiteHTTPTransport(client)
	if err != nil {
		return nil, err
	}
	return discoveryHTTP{host, refusal}, nil
}
