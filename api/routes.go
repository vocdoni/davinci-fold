package api

import "net/http"

// HTTP endpoint paths. {id} is an election ID, {voteID} a vote identifier,
// {workerID} a worker ID.
const (
	PingEndpoint = "/ping"
	InfoEndpoint = "/info"

	// Elections
	ElectionsEndpoint      = "/elections"             // POST (admin), GET
	ElectionEndpoint       = "/elections/{id}"        // GET
	ElectionStatusEndpoint = "/elections/{id}/status" // POST (admin)
	VotesEndpoint          = "/elections/{id}/votes"  // POST (self-authenticating)
	VoteEndpoint           = "/elections/{id}/votes/{voteID}"

	// Results / finalize handshake
	EncryptedResultsEndpoint = "/elections/{id}/encrypted-results" // GET (keywarden)
	DecryptionKeyEndpoint    = "/elections/{id}/decryption-key"    // POST (keywarden)
	ResultsEndpoint          = "/elections/{id}/results"           // GET

	// Worker pool
	WorkersEndpoint        = "/workers"            // GET
	WorkerRegisterEndpoint = "/workers/register"   // POST (admin)
	WorkerEndpoint         = "/workers/{workerID}" // DELETE (admin)
)

// URL parameter names.
const (
	ElectionURLParam = "id"
	VoteURLParam     = "voteID"
	WorkerURLParam   = "workerID"
)

// LogRedactedRoutes are the routes, as "METHOD pattern", whose request and
// response bodies are never logged: they carry secrets.
// Their handlers must never interpolate request values into error messages.
var LogRedactedRoutes = []string{
	http.MethodPost + " " + DecryptionKeyEndpoint,
}
