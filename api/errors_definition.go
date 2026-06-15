//nolint:lll
package api

import (
	"fmt"
	"net/http"
)

// Error codes in the 40001-49999 range are the caller's fault (HTTP 4xx).
// Error codes 50001-59999 are the server's fault (HTTP 5xx).
// NEVER change an existing error code; only append new ones after the last 4XXX
// or 5XXX. If you notice a gap, DON'T fill it: that code was retired.
var (
	ErrResourceNotFound          = Error{Code: 40001, HTTPstatus: http.StatusNotFound, Err: fmt.Errorf("resource not found")}
	ErrMalformedBody             = Error{Code: 40002, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("malformed JSON body")}
	ErrMalformedParam            = Error{Code: 40003, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("malformed parameter")}
	ErrUnauthorized              = Error{Code: 40004, HTTPstatus: http.StatusForbidden, Err: fmt.Errorf("unauthorized")}
	ErrInvalidToken              = Error{Code: 40005, HTTPstatus: http.StatusUnauthorized, Err: fmt.Errorf("invalid or expired token")}
	ErrElectionNotFound          = Error{Code: 40006, HTTPstatus: http.StatusNotFound, Err: fmt.Errorf("election not found")}
	ErrElectionExists            = Error{Code: 40007, HTTPstatus: http.StatusConflict, Err: fmt.Errorf("election already exists")}
	ErrElectionNotAcceptingVotes = Error{Code: 40008, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("election is not accepting votes")}
	ErrInvalidBallotProof        = Error{Code: 40009, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("invalid ballot proof")}
	ErrInvalidSignature          = Error{Code: 40010, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("invalid signature")}
	ErrInvalidCensusProof        = Error{Code: 40011, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("invalid census proof")}
	ErrVoteAlreadySubmitted      = Error{Code: 40012, HTTPstatus: http.StatusConflict, Err: fmt.Errorf("vote already submitted")}
	ErrVoteNotFound              = Error{Code: 40013, HTTPstatus: http.StatusNotFound, Err: fmt.Errorf("vote not found")}
	ErrResultsNotReady           = Error{Code: 40014, HTTPstatus: http.StatusConflict, Err: fmt.Errorf("results not ready")}
	ErrWorkerNotFound            = Error{Code: 40015, HTTPstatus: http.StatusNotFound, Err: fmt.Errorf("worker not found")}
	ErrMalformedWorkerInfo       = Error{Code: 40016, HTTPstatus: http.StatusBadRequest, Err: fmt.Errorf("malformed worker info")}

	ErrMarshalingServerJSONFailed = Error{Code: 50001, HTTPstatus: http.StatusInternalServerError, Err: fmt.Errorf("marshaling (server-side) JSON failed")}
	ErrGenericInternalServerError = Error{Code: 50002, HTTPstatus: http.StatusInternalServerError, Err: fmt.Errorf("internal server error")}
)
