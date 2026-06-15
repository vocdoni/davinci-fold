package api

// HTTP endpoint paths. {id} is an election ID, {voteID} a vote identifier.
const (
	PingEndpoint = "/ping"
	InfoEndpoint = "/info"

	// Elections
	ElectionsEndpoint = "/elections"            // POST (admin), GET
	ElectionEndpoint  = "/elections/{id}"       // GET
	VotesEndpoint     = "/elections/{id}/votes" // POST (self-authenticating)
	VoteEndpoint      = "/elections/{id}/votes/{voteID}"

	// Results / finalize handshake
	EncryptedResultsEndpoint = "/elections/{id}/encrypted-results" // GET (keywarden)
	DecryptionKeyEndpoint    = "/elections/{id}/decryption-key"    // POST (keywarden)
	ResultsEndpoint          = "/elections/{id}/results"           // GET

	// Worker pool
	WorkersEndpoint        = "/workers"          // GET
	WorkerRegisterEndpoint = "/workers/register" // POST (admin)
)

// URL parameter names.
const (
	ElectionURLParam = "id"
	VoteURLParam     = "voteID"
)
