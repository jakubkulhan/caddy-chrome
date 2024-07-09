package caddy_chrome

import (
	"bytes"
	"net/http"
)

type response interface {
	Status() int
	Header() http.Header
	Buffer() *bytes.Buffer
}

type responseWriter struct {
	status int
	header http.Header
	buffer bytes.Buffer
}

func (r *responseWriter) Status() int {
	return r.status
}

func (r *responseWriter) Header() http.Header {
	return r.header
}

func (r *responseWriter) Write(data []byte) (int, error) {
	return r.buffer.Write(data)
}

func (r *responseWriter) WriteHeader(statusCode int) {
	r.status = statusCode
}

func (r *responseWriter) Buffer() *bytes.Buffer {
	return &r.buffer
}

var (
	_ http.ResponseWriter = (*responseWriter)(nil)
	_ response            = (*responseWriter)(nil)
)
