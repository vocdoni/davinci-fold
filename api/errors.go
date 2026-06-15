package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/vocdoni/davinci-node/log"
)

// Error is used by handler functions to wrap errors, assigning a unique error
// code and specifying which HTTP status should be used.
type Error struct {
	Err        error
	Code       int
	HTTPstatus int
}

// MarshalJSON returns a JSON containing Err.Error() and Code. HTTPstatus is ignored.
//
// Example output: {"error":"resource not found","code":40001}
func (e Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(
		struct {
			Err  string `json:"error"`
			Code int    `json:"code"`
		}{
			Err: func() string {
				if e.Err == nil {
					return "(empty)"
				}
				return e.Err.Error()
			}(),
			Code: e.Code,
		})
}

// Error returns the message contained inside the Error.
func (e Error) Error() string {
	return e.Err.Error()
}

// Write serializes a JSON message using Error.Err and Error.Code and writes it.
func (e Error) Write(w http.ResponseWriter) {
	msg, err := json.Marshal(e)
	if err != nil {
		log.Warn(err)
		http.Error(w, "marshal failed", http.StatusInternalServerError)
		return
	}
	if log.Level() == log.LogLevelDebug {
		log.Debugw("API error response", "error", e.Error(), "code", e.Code, "httpStatus", e.HTTPstatus)
	}
	w.Header().Set("Content-Type", "application/json")
	http.Error(w, string(msg), e.HTTPstatus)
}

// Withf returns a copy of Error with the Sprintf-formatted string appended.
func (e Error) Withf(format string, args ...any) Error {
	return Error{
		Err:        fmt.Errorf("%w: %v", e.Err, fmt.Sprintf(format, args...)),
		Code:       e.Code,
		HTTPstatus: e.HTTPstatus,
	}
}

// With returns a copy of Error with the string appended.
func (e Error) With(s string) Error {
	return Error{
		Err:        fmt.Errorf("%w: %v", e.Err, s),
		Code:       e.Code,
		HTTPstatus: e.HTTPstatus,
	}
}

// WithErr returns a copy of Error with err.Error() appended.
func (e Error) WithErr(err error) Error {
	return Error{
		Err:        fmt.Errorf("%w: %v", e.Err, err.Error()),
		Code:       e.Code,
		HTTPstatus: e.HTTPstatus,
	}
}
