package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"
)

type ServiceConfig struct {
	Backend Backend
	// TODO(sean) make queue size part of config
}

// Service keeps the service configuration for the SDR API service.
type Service struct {
	backend      Backend
	requestQueue chan struct{}
}

func NewService(config *ServiceConfig) *Service {
	return &Service{
		backend:      config.Backend,
		requestQueue: make(chan struct{}, 10),
	}
}

// ServeHTTP parses a query request, translates and forwards it to InfluxDB
// and writes the results back to the client.
func (svc *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// wait for availability in request queue
	select {
	case svc.requestQueue <- struct{}{}:
	case <-time.After(10 * time.Second):
		http.Error(w, "error: service unavailable. too many active requests.", http.StatusServiceUnavailable)
		return
	}
	defer func() {
		<-svc.requestQueue
	}()

	query, err := parseQuery(r.Body)
	if err == io.EOF {
		http.Error(w, "error: must provide a request body", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("error: failed to parse query: %s", err.Error()), http.StatusBadRequest)
		return
	}

	queryStart := time.Now()
	queryCount := 0

	results, err := svc.backend.Query(r.Context(), query)
	if err != nil {
		log.Printf("error: failed to query backend: %s", err.Error())
		http.Error(w, fmt.Sprintf("error: failed to query backend: %s", err.Error()), http.StatusInternalServerError)
		return
	}
	defer results.Close()

	w.Header().Add("Access-Control-Allow-Origin", "*")
	writeContentDispositionHeader(w)
	w.WriteHeader(http.StatusOK)

	for results.Next() {
		if err := writeRecord(w, results.Record()); err != nil {
			break
		}
		queryCount++
	}

	if err := results.Err(); err != nil {
		log.Printf("error: %s", err)
	}

	queryDuration := time.Since(queryStart)
	log.Printf("served %d records in %s - %f records/s", queryCount, queryDuration, float64(queryCount)/queryDuration.Seconds())
}

var metaRE = regexp.MustCompile("^[a-zA-Z_][a-zA-Z0-9_]*$")

func parseQuery(r io.Reader) (*Query, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	query := &Query{}
	if err := decoder.Decode(query); err != nil {
		return nil, err
	}
	if query.Start == "" {
		return nil, fmt.Errorf("missing start field")
	}
	for k := range query.Filter {
		if !metaRE.MatchString(k) {
			return nil, fmt.Errorf("invalid filter key: %q", k)
		}
	}
	return query, nil
}

func writeRecord(w io.Writer, rec *Record) error {
	return json.NewEncoder(w).Encode(rec)
}

func writeContentDispositionHeader(w http.ResponseWriter) {
	filename := time.Now().Format("sage-download-20060102150405.ndjson")
	w.Header().Add("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
}
