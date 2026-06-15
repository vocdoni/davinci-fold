package api

import (
	"math/big"
	"net/http"
	"strings"

	"github.com/vocdoni/davinci-fold/types"
)

// encryptedResults serves the encrypted results ciphertext for the keywarden.
// GET /elections/{id}/encrypted-results (keywarden)
func (a *API) encryptedResults(w http.ResponseWriter, r *http.Request) {
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	ct, err := a.engine.EncryptedResults(id)
	if err != nil {
		ErrResultsNotReady.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, &EncryptedResultsResponse{
		ElectionID: id.String(),
		Ciphertext: ct,
	})
}

// decryptionKey receives the results decryption key and triggers finalize.
// POST /elections/{id}/decryption-key (keywarden)
func (a *API) decryptionKey(w http.ResponseWriter, r *http.Request) {
	subject := subjectFromContext(r.Context())
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	var req DecryptionKeyRequest
	if err := httpReadJSON(r, &req); err != nil {
		ErrMalformedBody.WithErr(err).Write(w)
		return
	}
	key, ok := new(big.Int).SetString(strings.TrimPrefix(req.Key, "0x"), 16)
	if !ok {
		ErrMalformedParam.With("key must be 0x big-endian hex").Write(w)
		return
	}
	res, err := a.engine.SubmitDecryptionKey(subject, id, key)
	if err != nil {
		ErrResultsNotReady.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, resultsResponse(res))
}

// results returns the final tally and PLONK snark. GET /elections/{id}/results
func (a *API) results(w http.ResponseWriter, r *http.Request) {
	id, err := electionIDFromURL(r)
	if err != nil {
		ErrMalformedParam.WithErr(err).Write(w)
		return
	}
	res, err := a.engine.Results(id)
	if err != nil {
		ErrResultsNotReady.WithErr(err).Write(w)
		return
	}
	httpWriteJSON(w, resultsResponse(res))
}

// resultsResponse maps stored Results to the API view.
func resultsResponse(res *types.Results) *ResultsResponse {
	return &ResultsResponse{
		ElectionID:       res.ElectionID.String(),
		Tally:            res.Tally,
		ProgramVK:        res.ProgramVK,
		RootCVadcopFinal: res.RootCVadcopFinal,
		PublicValues:     res.PublicValues,
		ProofBytes:       res.ProofBytes,
	}
}
