package api

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/types"
)

// createElection creates a new election. POST /elections (admin)
func (a *API) createElection(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromContext(r.Context())

	var req ElectionCreateRequest
	if err := httpReadJSON(r, &req); err != nil {
		ErrMalformedBody.WithErr(err).Write(w)
		return
	}
	id, err := hex.DecodeString(strings.TrimPrefix(req.ProcessID, "0x"))
	if err != nil || len(id) == 0 {
		ErrMalformedParam.With("processID must be non-empty hex").Write(w)
		return
	}

	el := &types.Election{
		ID:        types.ElectionID(id),
		BatchSize: req.BatchSize,
		FoldEvery: req.FoldEvery,
		EndTime:   req.EndTime,
		Config: types.ElectionConfig{
			ProcessID:    req.ProcessID,
			BallotMode:   req.BallotMode,
			EncX:         req.EncX,
			EncY:         req.EncY,
			CensusOrigin: req.CensusOrigin,
			CensusRoot:   req.CensusRoot,
			VK:           req.VK,
		},
	}
	if err := a.engine.CreateElection(subject, el); err != nil {
		if errors.Is(err, storage.ErrKeyAlreadyExists) {
			ErrElectionExists.WithErr(err).Write(w)
			return
		}
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	got, err := a.engine.Election(el.ID)
	if err != nil {
		ErrGenericInternalServerError.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, electionResponse(got))
}

// listElections returns all known elections. GET /elections
func (a *API) listElections(w http.ResponseWriter, _ *http.Request) {
	els, err := a.engine.ListElections()
	if err != nil {
		ErrGenericInternalServerError.WithErr(err).Write(w)
		return
	}
	resp := &ElectionsResponse{Elections: make([]*ElectionResponse, 0, len(els))}
	for _, el := range els {
		resp.Elections = append(resp.Elections, electionResponse(el))
	}
	httpWriteJSON(w, resp)
}

// getElection returns a single election. GET /elections/{id}
func (a *API) getElection(w http.ResponseWriter, r *http.Request) {
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	el, err := a.engine.Election(id)
	if err != nil {
		ErrElectionNotFound.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, electionResponse(el))
}

// settableStatuses are the statuses an organizer can request, by name.
var settableStatuses = map[string]types.Status{
	types.StatusActive.String():   types.StatusActive,
	types.StatusPaused.String():   types.StatusPaused,
	types.StatusEnded.String():    types.StatusEnded,
	types.StatusCanceled.String(): types.StatusCanceled,
}

// setElectionStatus pauses, resumes, ends or cancels an election.
// POST /elections/{id}/status (admin)
func (a *API) setElectionStatus(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromContext(r.Context())
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	var req ElectionStatusRequest
	if err := httpReadJSON(r, &req); err != nil {
		ErrMalformedBody.WithErr(err).Write(w)
		return
	}
	status, ok := settableStatuses[req.Status]
	if !ok {
		ErrMalformedParam.Withf("status must be active, paused, ended or canceled, got %q", req.Status).Write(w)
		return
	}
	if err := a.engine.SetStatus(subject, id, status); err != nil {
		engineError(err).Write(w)
		return
	}
	el, err := a.engine.Election(id)
	if err != nil {
		ErrGenericInternalServerError.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, electionResponse(el))
}

// electionResponse maps a stored election to its public view.
func electionResponse(el *types.Election) *ElectionResponse {
	return &ElectionResponse{
		ID:            el.ID.String(),
		Status:        el.Status.String(),
		BatchSize:     el.BatchSize,
		FoldEvery:     el.FoldEvery,
		EndTime:       el.EndTime.UTC(),
		CreatedAt:     el.CreatedAt.UTC(),
		FinalizeError: el.FinalizeError,
	}
}
