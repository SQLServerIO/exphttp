// Package exphttp implements HTTP request/response timing collection for the
// standard `net/http` package.
//
// Example usage:
//     func getThePage(w http.ResponseWriter, r *http.Request) {
//        fmt.Fprint(w, "neat page")
//     }
//
//     http.HandleFunc("/thepage/", getThePage)
//
//     // same page, but now with stats tracked!
//     http.Handle("/thepage2/", exphttp.NewExpHandler("thepage2", MakeExpHandlerFunc(getThePage)))
//
// Although it's more efficient with a slightly tweaked http.HandlerFunc that
// returns the response status code. Here the handler is modified to return its
// status code work with exphttp:
//     func getThePage2(w http.ResponseWriter, r *http.Request) int {
//        fmt.Fprint(w, "neat page")
//        return http.StatusOK
//     }
//
//     http.Handle("/thepage2/", exphttp.NewExpHandler("thepage2", getThePage2))
//
package exphttp

import (
	"expvar"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// DefaultGranularity is the default level of granularity for recording
// counters over time. For example, a RateCounter with an interval of 1 minute
// and a granulartiy of 30 will be accurate to within 2 seconds.
//
// Thus you want to make sure your polling interval is greater than the
// measurement interval divided by granularity.
const DefaultGranularity = 32

// DefaultLogger is used when creating new ExpHandlers, and used to log requests
// and timing info to Stderr.
//
// Set to nil to disable before calling NewExpHandler()
var DefaultLogger = log.New(os.Stderr, "", log.LstdFlags)

var expHandlers *expvar.Map

// ExpHandlerFunc is a http.HandlerFunc that returns it's own HTTP StatusCode.
type ExpHandlerFunc func(w http.ResponseWriter, r *http.Request) int

type getStatusCode struct {
	http.ResponseWriter
	code int
}

func (w *getStatusCode) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

// MakeExpHandlerFunc wraps a http.HandlerFunc so that the response status code
// is accessible. It is more efficient to update your code to implement
// ExpHandlerFunc and return the code directly.
func MakeExpHandlerFunc(h http.HandlerFunc) ExpHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) int {
		w2 := &getStatusCode{w, 500}
		h(w2, r)
		return w2.code
	}
}

// ExpHandler is an http.Handler that exposes request/response timing
// information via the `expvar` stdlib package.
type ExpHandler struct {
	// Name of the handler/endpoint.
	Name string

	// Stats contains the request/response stats that are exposed.
	Stats *expvar.Map

	// Durations are the time spans for the rate counters. Only parsed once in
	// the first incoming request to populate counters.
	Durations map[string]time.Duration

	// HandlerFunc is the ExpHandlerFunc that is tracked.
	HandlerFunc ExpHandlerFunc

	// Log requests to this logger if non-nil.
	Log *log.Logger

	didInit      bool
	reqCounters  []*RateCounter
	respCounters []*RateCounter
}

// NewExpHandler creates a new ExpHandler, publishes a new expvar.Map to track
// it, sets a default Durations={"min": time.Minute}, sets Log=DefaultLogger,
// and adds name to the exposed "exphttp" map so that stats polling code
// can auto-discover.
func NewExpHandler(name string, h ExpHandlerFunc) *ExpHandler {
	if expHandlers == nil {
		expHandlers = expvar.NewMap("exphttp")
	}
	e := &ExpHandler{
		Name:        name,
		Stats:       expvar.NewMap(name),
		Durations:   map[string]time.Duration{"min": time.Minute},
		HandlerFunc: h,
		Log:         DefaultLogger,
	}

	expHandlers.Add(name, 1)
	return e
}

func (e *ExpHandler) init() {
	e.reqCounters = make([]*RateCounter, 0, len(e.Durations))
	e.respCounters = make([]*RateCounter, 0, len(e.Durations))

	for key, dur := range e.Durations {
		r1 := NewRateCounter(dur)
		r2 := NewRateCounter(dur)
		e.Stats.Set("requests_per_"+key, r1)
		e.Stats.Set("responses_per_"+key, r2)
		e.reqCounters = append(e.reqCounters, r1)
		e.respCounters = append(e.respCounters, r2)
	}
	e.didInit = true
}

// ServeHTTP implements the http.Handler interface.
func (e *ExpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !e.didInit {
		e.init()
	}

	e.Stats.Add("requests", 1)
	for _, rc := range e.reqCounters {
		rc.Add(1)
	}

	startTime := time.Now()
	defer func() {
		if p := recover(); p != nil {
			elap := time.Now().Sub(startTime).Nanoseconds()

			if e.Log != nil {
				e.Log.Println("caught panic: ", p)
			}
			e.Stats.Add("panics", 1)
			e.Stats.Add("responses", 1)
			for _, rc := range e.respCounters {
				rc.Add(1)
			}
			e.Stats.Add("responses.500", 1)
			e.Stats.Add("responses.500.total_ns", elap)

			http.Error(w, "server error", http.StatusInternalServerError)
		}
	}()
	////////

	code := e.HandlerFunc(w, r)

	////////
	elapsed := time.Now().Sub(startTime).Nanoseconds()
	if e.Log != nil {
		e.Log.Println(float64(elapsed)/1000000.0, "ms --", code, "--", r.Method, r.URL)
	}

	e.Stats.Add("responses", 1)
	for _, rc := range e.respCounters {
		rc.Add(1)
	}

	switch code {
	case http.StatusOK:
		e.Stats.Add("responses.200", 1)
		e.Stats.Add("responses.200.total_ns", elapsed)
	case http.StatusBadRequest:
		e.Stats.Add("responses.400", 1)
		e.Stats.Add("responses.400.total_ns", elapsed)
	case http.StatusUnauthorized:
		e.Stats.Add("responses.401", 1)
		e.Stats.Add("responses.401.total_ns", elapsed)
	case http.StatusInternalServerError:
		e.Stats.Add("responses.500", 1)
		e.Stats.Add("responses.500.total_ns", elapsed)
	default:
		e.Stats.Add(fmt.Sprintf("responses.%d", code), 1)
		e.Stats.Add(fmt.Sprintf("responses.%d.total_ns", code), elapsed)
	}
}
