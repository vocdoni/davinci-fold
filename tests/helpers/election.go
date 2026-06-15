package helpers

import (
	"math/big"
	"os"
	"strings"
	"time"

	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"

	"github.com/vocdoni/davinci-fold/api"
)

// NewElectionRequest builds a valid create-election body bound to a fresh
// ElGamal key, returning the request and the private scalar the keywarden
// returns as the v1 decryption key at finalize.
func NewElectionRequest(processID string, batchSize, foldEvery int, endTime time.Time) (*api.ElectionCreateRequest, *big.Int, error) {
	pub, priv, err := elgamal.GenerateKey(bjjgnark.New())
	if err != nil {
		return nil, nil, err
	}
	rx, ry := pub.(*bjjgnark.BJJ).Point()
	req := &api.ElectionCreateRequest{
		ProcessID:  processID,
		BallotMode: "0x01",
		EncX:       "0x" + rx.Text(16),
		EncY:       "0x" + ry.Text(16),
		CensusRoot: "0x1234",
		BatchSize:  batchSize,
		FoldEvery:  foldEvery,
		EndTime:    endTime,
	}
	return req, priv, nil
}

// WorkerURLsFromEnv parses DAVINCI_FOLD_WORKER_URLS (comma-separated prover
// base URLs) for the GPU-backed integration tests.
func WorkerURLsFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("DAVINCI_FOLD_WORKER_URLS"))
	if raw == "" {
		return nil
	}
	var urls []string
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}
