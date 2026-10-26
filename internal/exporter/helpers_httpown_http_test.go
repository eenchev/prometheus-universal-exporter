//go:build !select_request_types || request_type_http

package exporter

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// httpOwn edits an http request's own keys.
func httpOwn(r *model.RequestConfig, edit func(*fetch.HTTPConfig)) {
	own, _ := r.Own.(fetch.HTTPConfig)
	edit(&own)
	r.Own = own
}

// editHTTP edits an http collector's own keys.
func editHTTP(c *model.Collector, edit func(*fetch.HTTPConfig)) {
	httpOwn(&c.Request, edit)
}
