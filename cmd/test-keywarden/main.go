// Command test-keywarden is the v1 local keywarden for davinci-fold. It owns the
// election encryption keypair: it generates one (keygen), prints the public key
// for election creation, and on demand fetches an election's encrypted results
// ciphertext from the orchestrator and returns the decryption key (finalize).
//
// It stands in for a future on-chain DKG. The private key lives only here and in
// the keyfile; the orchestrator never receives it until the keywarden chooses to
// release it at finalize. v1's "decryption key" is the raw ElGamal private
// scalar; a DKG variant will return threshold shares through the same handshake.
package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	flag "github.com/spf13/pflag"

	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"
	"github.com/vocdoni/davinci-node/log"

	"github.com/vocdoni/davinci-fold/internal"
	"github.com/vocdoni/davinci-fold/keywarden"
)

// keyFile is the persisted keypair: the public RTE coordinates published for
// election creation and the private scalar released at finalize. All big-endian
// hex (0x-prefixed).
type keyFile struct {
	EncX string `json:"encX"`
	EncY string `json:"encY"`
	Priv string `json:"priv"`
}

func main() {
	mode := flag.String("mode", "", "keygen | finalize")
	keyPath := flag.String("keyfile", "keywarden-key.json", "keypair file path")
	orchestrator := flag.String("orchestrator", "http://127.0.0.1:8080", "orchestrator base URL (finalize)")
	token := flag.String("token", "", "keywarden bearer token (finalize)")
	election := flag.String("election", "", "election ID hex (finalize)")
	flag.Parse()

	log.Init("info", "stdout", nil)

	switch *mode {
	case "keygen":
		if err := keygen(*keyPath); err != nil {
			log.Fatalf("keygen failed: %v", err)
		}
	case "finalize":
		if err := finalize(*keyPath, *orchestrator, *token, *election); err != nil {
			log.Fatalf("finalize failed: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "test-keywarden v%s\nusage: --mode=keygen|finalize\n", internal.Version)
		os.Exit(2)
	}
}

// keygen generates a fresh BabyJubJub ElGamal keypair, writes it to keyPath, and
// prints the public coordinates to embed in an election's config (EncX/EncY).
func keygen(keyPath string) error {
	pub, priv, err := elgamal.GenerateKey(bjjgnark.New())
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	x, y := pub.(*bjjgnark.BJJ).Point()
	kf := keyFile{
		EncX: "0x" + x.Text(16),
		EncY: "0x" + y.Text(16),
		Priv: "0x" + priv.Text(16),
	}
	blob, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, blob, 0o600); err != nil {
		return fmt.Errorf("write keyfile: %w", err)
	}
	log.Infow("generated election keypair", "keyfile", keyPath, "encX", kf.EncX, "encY", kf.EncY)
	fmt.Printf("encX=%s\nencY=%s\n", kf.EncX, kf.EncY)
	return nil
}

// finalize fetches the election's published ciphertext (completing the keywarden
// handshake) and returns the decryption key, triggering the orchestrator's
// finalize. v1 releases the raw private scalar.
func finalize(keyPath, orchestrator, token, election string) error {
	if election == "" {
		return fmt.Errorf("--election is required")
	}
	blob, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read keyfile: %w", err)
	}
	var kf keyFile
	if err := json.Unmarshal(blob, &kf); err != nil {
		return fmt.Errorf("decode keyfile: %w", err)
	}
	priv, ok := new(big.Int).SetString(trim0x(kf.Priv), 16)
	if !ok {
		return fmt.Errorf("invalid private scalar in keyfile")
	}

	c := keywarden.NewClient(orchestrator, token)
	ct, err := c.EncryptedResults(election)
	if err != nil {
		return fmt.Errorf("fetch encrypted results: %w", err)
	}
	log.Infow("fetched encrypted results", "election", election, "ciphertext", len(ct.Ciphertext))

	if err := c.SubmitDecryptionKey(election, priv); err != nil {
		return fmt.Errorf("submit decryption key: %w", err)
	}
	log.Infow("submitted decryption key", "election", election)
	return nil
}

func trim0x(s string) string {
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}
