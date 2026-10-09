package helps

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexHTTPCompressionNegotiationAndDecode(t *testing.T) {
	for _, protocol := range []string{"h2", "http/1.1", ""} {
		for _, encoding := range []string{"", "gzip"} {
			t.Run(protocol+"/response="+encoding, func(t *testing.T) {
				payload := "data: {\"type\":\"response.completed\"}\n\n"
				tlsConn, tracked, serverURL := newALPNTestConnection(t, protocol, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, present := r.Header["Accept-Encoding"]; present {
						t.Errorf("unexpected Accept-Encoding: %q", r.Header.Values("Accept-Encoding"))
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if encoding == "gzip" {
						w.Header().Set("Content-Encoding", "gzip")
						writer := gzip.NewWriter(w)
						_, _ = io.WriteString(writer, payload)
						_ = writer.Close()
					} else {
						_, _ = io.WriteString(w, payload)
					}
				}))
				transport := &codexResponseTransport{next: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return roundTripUtlsConnection(req, tlsConn, true)
				})}
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, serverURL, strings.NewReader("{}"))
				resp, err := transport.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				if err != nil || string(got) != payload {
					t.Fatalf("body=%q error=%v", got, err)
				}
				if resp.Header.Get("Content-Encoding") != "" || resp.Uncompressed != (encoding == "gzip") {
					t.Fatal("decoded response metadata is inconsistent")
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				tracked.requireClosed(t)
			})
		}
	}
}

func TestCodexHTTPDefaultAndExplicitGzip(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want := ""
			if explicit {
				want = "gzip"
			}
			if got := r.Header.Get("Accept-Encoding"); got != want {
				t.Errorf("Accept-Encoding=%q want %q", got, want)
			}
			w.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(w)
			_, _ = io.WriteString(writer, "decoded")
			_ = writer.Close()
		}))
		client := NewCodexHTTPClient(t.Context(), nil, nil, 0)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		if explicit {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		client.CloseIdleConnections()
		server.Close()
		if err != nil || string(body) != "decoded" {
			t.Fatalf("decode: %q %v", body, err)
		}
	}
	if http.DefaultTransport.(*http.Transport).DisableCompression {
		t.Fatal("shared default transport was mutated")
	}
	generic := NewUtlsHTTPClient(t.Context(), nil, nil, 0).Transport.(*fallbackRoundTripper)
	if generic.chrome.(*utlsRoundTripper).disableCompression {
		t.Fatal("other providers lost default negotiation")
	}
}

func TestCodexGzipBodyErrorsCloseAndCustomTransport(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		var wire bytes.Buffer
		writer := gzip.NewWriter(&wire)
		_, _ = writer.Write([]byte("payload"))
		_ = writer.Close()
		if corrupt {
			wire.Reset()
			wire.WriteString("invalid gzip stream")
		}
		body := &trackedReadCloser{Reader: bytes.NewReader(wire.Bytes())}
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"123"}}, Body: body, Request: req}, nil
		}))
		client := NewCodexHTTPClient(ctx, nil, nil, 0)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/test", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		if (err != nil) != corrupt {
			t.Fatalf("corrupt=%v err=%v", corrupt, err)
		}
		_ = resp.Body.Close()
		if body.closeCount != 1 {
			t.Fatal("underlying body was not closed")
		}
		if resp.ContentLength != -1 || resp.Header.Get("Content-Length") != "" {
			t.Fatal("compressed length retained after decoding")
		}
	}
	body := &trackedReadCloser{Reader: strings.NewReader("")}
	_ = (&codexGzipBody{source: body}).Close()
	if body.closeCount != 1 {
		t.Fatal("unread body was not closed")
	}
}
