package api

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/vocdoni/davinci-node/log"

	"github.com/vocdoni/davinci-fold/types"
)

// maxRequestBody bounds a decoded request body (1 MiB), large enough for a
// ballot proof + census proof but a hard ceiling against oversized payloads.
const maxRequestBody = 1 << 20

// httpReadJSON decodes a length-limited JSON request body into out.
func httpReadJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	return dec.Decode(out)
}

// electionIDFromURL parses the hex {id} path parameter into an ElectionID.
func electionIDFromURL(r *http.Request) (types.ElectionID, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(chi.URLParam(r, ElectionURLParam), "0x"))
	if err != nil {
		return nil, err
	}
	return types.ElectionID(b), nil
}

// voteIDFromURL parses the hex {voteID} path parameter into a VoteID.
func voteIDFromURL(r *http.Request) (types.VoteID, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(chi.URLParam(r, VoteURLParam), "0x"))
	if err != nil {
		return nil, err
	}
	return types.VoteID(b), nil
}

// DisabledLogging is a global flag to disable the logging middleware.
var DisabledLogging = false

// httpWriteJSON writes a JSON response.
func httpWriteJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	jdata, err := json.Marshal(data)
	if err != nil {
		ErrMarshalingServerJSONFailed.WithErr(err).Write(w)
		return
	}
	n, err := w.Write(jdata)
	if err != nil {
		log.Warnw("failed to write http response", "error", err)
		return
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		log.Warnw("failed to write on response", "error", err)
		return
	}
	if !DisabledLogging && log.Level() == log.LogLevelDebug {
		log.Debugw("api response", "bytes", n, "data", strings.ReplaceAll(string(jdata), "\"", ""))
	}
}

// httpWriteOK writes an empty OK response.
func httpWriteOK(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("\n")); err != nil {
		log.Warnw("failed to write on response", "error", err)
	}
}
