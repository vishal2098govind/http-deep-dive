package client

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
)

func Foo() {
	cli := http.Client{
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				panic("TODO")
			},
			OnProxyConnectResponse: func(ctx context.Context, proxyURL *url.URL, connectReq *http.Request, connectRes *http.Response) error {
				panic("TODO")
			},
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				panic("TODO")
			},
			Dial: func(network string, addr string) (net.Conn, error) {
				panic("TODO")
			},
			DialTLSContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				panic("TODO")
			},
			DialTLS: func(network string, addr string) (net.Conn, error) {
				panic("TODO")
			},
			TLSClientConfig:       &tls.Config{},
			TLSHandshakeTimeout:   0,
			DisableKeepAlives:     false,
			DisableCompression:    false,
			MaxIdleConns:          0,
			MaxIdleConnsPerHost:   0,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       0,
			ResponseHeaderTimeout: 0,
			ExpectContinueTimeout: 0,
			TLSNextProto:          map[string]func(authority string, c *tls.Conn) http.RoundTripper{},
			ProxyConnectHeader:    http.Header{},
			GetProxyConnectHeader: func(ctx context.Context, proxyURL *url.URL, target string) (http.Header, error) {
				panic("TODO")
			},
			MaxResponseHeaderBytes: 0,
			WriteBufferSize:        0,
			ReadBufferSize:         0,
			ForceAttemptHTTP2:      false,
			HTTP2:                  &http.HTTP2Config{},
			Protocols:              &http.Protocols{},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			panic("TODO")
		},
		Jar:     nil,
		Timeout: 0,
	}
}
