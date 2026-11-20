package adapter

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	streamWriteTimeout    = 30 * time.Second
	upstreamHeaderTimeout = 2 * time.Minute
	requestReadTimeout    = 2 * time.Minute
)

type deadlineResponseWriter struct {
	http.ResponseWriter
}

func (w deadlineResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w deadlineResponseWriter) setDeadline() error {
	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w deadlineResponseWriter) Write(body []byte) (int, error) {
	if err := w.setDeadline(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(body)
}

func (w deadlineResponseWriter) FlushError() error {
	if err := w.setDeadline(); err != nil {
		return err
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func requestsUpgrade(headers http.Header) bool {
	for _, value := range headers.Values("Upgrade") {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	for _, value := range headers.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func removeHopHeaders(headers http.Header) {
	for _, value := range headers.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(token))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(name)
	}
}

func removeConditionalHeaders(headers http.Header) {
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since"} {
		headers.Del(name)
	}
}
