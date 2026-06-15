package api

import (
	"net/http"

	"github.com/vocdoni/davinci-fold/orchestrator"
)

// newVote ingests a self-authenticating vote (ballot proof + ECDSA + census).
// POST /elections/{id}/votes
func (a *API) newVote(w http.ResponseWriter, r *http.Request) {
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	var sub orchestrator.VoteSubmission
	if err := httpReadJSON(r, &sub); err != nil {
		ErrMalformedBody.WithErr(err).Write(w)
		return
	}
	v, err := a.engine.SubmitVote(id, &sub)
	if err != nil {
		ErrInvalidBallotProof.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, &VoteReceiptResponse{
		VoteID: v.ID.String(),
		Status: "accepted",
	})
}

// getVote returns the status of a single vote. GET /elections/{id}/votes/{voteID}
func (a *API) getVote(w http.ResponseWriter, r *http.Request) {
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	voteID, err := voteIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	v, st, err := a.engine.VoteRecord(id, voteID)
	if err != nil {
		ErrVoteNotFound.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, &VoteStatusResponse{
		VoteID: v.ID.String(),
		Status: st.String(),
		Seq:    v.Seq,
	})
}
