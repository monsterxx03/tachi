package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// identityFileName holds the node's key pair, inside its data dir.
const identityFileName = "identity.json"

// Identity is a node's stable identity: a key pair plus the node_id derived from
// its public half. The identity outlives restarts, addresses and hosting
// processes — persisted peer bindings key on NodeID, never on an alias or an IP.
type Identity struct {
	NodeID     string    `json:"node_id"`
	PublicKey  string    `json:"public_key"`  // base64(std) ed25519 public key
	PrivateKey string    `json:"private_key"` // base64(std) of the 32-byte seed
	CreatedAt  time.Time `json:"created_at"`
}

// NodeIDFor derives the stable node id from a public key.
//
// Derived rather than random on purpose: a random id sitting next to a key can
// drift from it (copied file, edited JSON), and a reader has no way to notice.
// Deriving means every read re-computes and verifies the binding, so a mismatched
// identity fails loudly instead of impersonating the peer it replaced.
func NodeIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// LoadOrCreateIdentity returns the identity in dir, creating one on first use.
//
// The file is written atomically with private permissions (0700 dir, 0600 file):
// the private key never lands in a world-readable path, not even for the instant
// a plain write would leave it there.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	id, err := LoadIdentity(dir)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("node: generate identity key: %w", err)
	}
	id = &Identity{
		NodeID:     NodeIDFor(pub),
		PublicKey:  base64.StdEncoding.EncodeToString(pub),
		PrivateKey: base64.StdEncoding.EncodeToString(priv.Seed()),
		CreatedAt:  time.Now().UTC(),
	}
	if err := id.save(dir); err != nil {
		return nil, err
	}
	return id, nil
}

// save is the ONE write path for identity.json.
func (i *Identity) save(dir string) error {
	if err := i.validate(); err != nil {
		return err
	}
	if err := fileutil.AtomicWriteJSONPrivate(filepath.Join(dir, identityFileName), i); err != nil {
		return fmt.Errorf("node: write identity: %w", err)
	}
	return nil
}

// LoadIdentity reads the identity in dir without creating one. A missing file
// reports os.ErrNotExist.
//
// The reading half is separate from the creating one so the attach path can use
// it: an attaching process must never mint a key into a data dir whose owner is
// already serving under a different one.
func LoadIdentity(dir string) (*Identity, error) {
	var id Identity
	if err := fileutil.ReadJSON(filepath.Join(dir, identityFileName), &id); err != nil {
		return nil, err
	}
	if err := id.validate(); err != nil {
		return nil, err
	}
	return &id, nil
}

func (i *Identity) validate() error {
	if i == nil {
		return errors.New("node: identity is nil")
	}
	if i.NodeID == "" || i.PublicKey == "" || i.PrivateKey == "" {
		return fmt.Errorf("node: identity in this data dir is incomplete (node_id=%q, public_key set=%t, private_key set=%t)",
			i.NodeID, i.PublicKey != "", i.PrivateKey != "")
	}

	// The three fields are one chain, not three facts: the seed DETERMINES the
	// public key, and the public key determines the node id. So the check runs
	// seed -> public -> node id, and a file that breaks anywhere in the chain is
	// refused — including the subtle one, a public key from one key pair grafted
	// next to the seed of another, which would otherwise pass a check that only
	// looked at each field against its neighbour.
	priv, err := i.SigningKey()
	if err != nil {
		return err
	}
	derivedPub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("node: identity private key has an unexpected public half")
	}
	statedPub, err := i.VerifyKey()
	if err != nil {
		return err
	}
	if !derivedPub.Equal(statedPub) {
		return errors.New("node: identity public_key is not the one its private_key produces")
	}
	if derived := NodeIDFor(derivedPub); derived != i.NodeID {
		return fmt.Errorf("node: identity node_id %s does not match its public key (derived %s)", i.NodeID, derived)
	}
	return nil
}

// SigningKey returns the private key. The TLS certificate is signed with it and this node's
// client certificate is made from the same key pair (node/tls.go), so it is the key behind every
// identity this node presents in either direction.
func (i *Identity) SigningKey() (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(i.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("node: decode private key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("node: private key seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// VerifyKey returns the public half.
func (i *Identity) VerifyKey() (ed25519.PublicKey, error) {
	pub, err := base64.StdEncoding.DecodeString(i.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("node: decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("node: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(pub), nil
}
