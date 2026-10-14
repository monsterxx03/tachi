package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// A node's TLS face: the certificate it serves, and how a caller satisfies itself that the
// machine answering is the machine it means to talk to.
//
// The certificate is SELF-SIGNED WITH THE NODE'S IDENTITY KEY (identity.json), and that is the
// whole trick rather than a shortcut: the node id is DERIVED from that key (NodeIDFor), so
// pinning a node id is pinning exactly this certificate's public key. Server authentication
// therefore costs no configuration at all — what a peer checks is the identity the node already
// had, and a certificate for a pinned node cannot be forged the way a name or a bearer token can.
//
// It is the SAME certificate in both directions, which is what makes this mutual TLS rather than
// a server certificate plus a shared secret: the node serves it on its remote surface and
// presents it as the client certificate when it is the one calling out (see makeCertificate's
// ExtKeyUsage). The accepting side therefore learns a KEY, and the node id it derives from that
// key is not something a caller can claim — a caller can generate as many keys as it likes, but
// it cannot present one that derives somebody else's id (verifyCallerCertificate).
//
// What the certificate deliberately does NOT say is anything a name would: it carries no subject
// alternative names, so a hostname is never verified, and the address a peer dials comes from
// that peer's config, not from the certificate. What is checked instead is the pin (TOFU,
// node/knownpeers.go) — which is why a peer may be configured by bare IP.

const (
	// tlsDirName holds the node's certificate, beside the identity key it was made from.
	tlsDirName   = "tls"
	certFileName = "identity.crt"

	// certValidity is long on purpose. The pin is the KEY, not the window, so an expiry date
	// buys nothing here except a node that has to be re-paired every year — which is a node that
	// will eventually be re-paired WRONG, by an operator in a hurry.
	certValidity = 10 * 365 * 24 * time.Hour
	// certBackdate absorbs clock skew between machines: a certificate that is "not yet valid"
	// because the peer's clock is a minute behind is a handshake failure nobody can explain.
	certBackdate = time.Hour
)

// loadOrCreateCertificate returns the node's TLS certificate, making one on first use.
//
// The stored certificate is derived data — the authority is the identity key — so one that is
// unreadable or made from a DIFFERENT key is replaced rather than reported: a node that rotated
// its identity must not become unbootable, and a certificate presenting an identity the node does
// not have is worse than no certificate at all.
func loadOrCreateCertificate(dataDir string, id *Identity) (tls.Certificate, error) {
	priv, err := id.SigningKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	pub, err := id.VerifyKey()
	if err != nil {
		return tls.Certificate{}, err
	}

	certPath := filepath.Join(dataDir, tlsDirName, certFileName)
	if raw, err := os.ReadFile(certPath); err == nil {
		if cert, err := certificateFromPEM(raw, priv, pub); err == nil {
			return cert, nil
		}
	}

	der, err := makeCertificate(priv, pub)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("node: parse the certificate just made: %w", err)
	}
	// Written the way the identity is: private dir, private file, one writer (fileutil).
	if err := fileutil.AtomicWriteFilePrivate(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return tls.Certificate{}, fmt.Errorf("node: write certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}

// certificateFromPEM reads a stored certificate, refusing one whose key is not this identity's.
//
// The check is the same chain identity.json itself is held to (validate): a certificate whose
// public key does not derive this node's id would present an identity the node does not have.
func certificateFromPEM(raw []byte, priv ed25519.PrivateKey, pub ed25519.PublicKey) (tls.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, errors.New("node: stored certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("node: parse stored certificate: %w", err)
	}
	stored, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !stored.Equal(pub) {
		return tls.Certificate{}, errors.New("node: stored certificate belongs to another identity")
	}
	// A certificate that cannot be presented in BOTH directions is not this node's certificate
	// any more — the surface would come up able to serve but unable to call out (or the reverse),
	// and the failure would look like a peer refusing us for no reason. This is derived data, so
	// the caller remakes it rather than reporting it (see loadOrCreateCertificate).
	if !hasPurpose(leaf, x509.ExtKeyUsageServerAuth) || !hasPurpose(leaf, x509.ExtKeyUsageClientAuth) {
		return tls.Certificate{}, errors.New("node: stored certificate is not usable in both directions")
	}
	return tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: priv, Leaf: leaf}, nil
}

// hasPurpose reports whether a certificate declares one extended key usage.
func hasPurpose(cert *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, eku := range cert.ExtKeyUsage {
		if eku == want {
			return true
		}
	}
	return false
}

// makeCertificate signs a self-signed certificate for this node's identity.
//
// No subject alternative names: the address a peer dials is the one its config names, and the
// check it performs is the PIN, not a hostname. A name here would suggest a verification that
// never happens.
func makeCertificate(priv ed25519.PrivateKey, pub ed25519.PublicKey) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("node: certificate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: NodeIDFor(pub), Organization: []string{"tachi"}},
		NotBefore:    now.Add(-certBackdate),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// BOTH directions: this one certificate is the node's identity wherever it appears — it
		// serves TLS on its own remote surface and it is the client certificate it presents when
		// it is the one calling out. One key, one certificate, no second material to manage.
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		// Deliberately NOT a CA: it signs nothing. A peer verifies it by PIN — it never builds a
		// chain — so claiming to be an authority would only invite a verification we do not do.
		IsCA: false,
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
}

// serverTLSConfig is what the remote surface listens with when it leaves the machine.
//
// A client certificate is REQUIRED, and any key is accepted — the two halves of that sentence are
// both deliberate:
//
//   - required, because the identity is what makes a task record say who asked. Without a
//     certificate there is no node id, and every record of inbound work would be anonymous.
//   - any key accepted, because this node does not decide who may call: the network does
//     (Options.checkRemoteListen). Turning a caller away is a judgment for whoever configured the
//     mesh, not for this process — and a whitelist here would be a second, invisible boundary that
//     nobody could see from the network's side.
//
// What is still verified is that the certificate is a CLIENT certificate and that it is valid
// now: those are facts about the certificate, not about its owner, and both are cheap.
func serverTLSConfig(cert tls.Certificate, log *logger.Logger) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyCallerCertificate(rawCerts, log)
		},
	}
}

// verifyCallerCertificate checks that the connection's client certificate is a usable one, and
// reports the node id it establishes.
//
// The id comes out of the KEY (NodeIDFor hashes the public half), which is what makes it
// unforgeable: a caller can generate as many keys as it likes, but it cannot present one that
// derives somebody else's node id.
func verifyCallerCertificate(rawCerts [][]byte, log *logger.Logger) error {
	if len(rawCerts) == 0 {
		return errors.New("node: a caller presented no certificate")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("node: a caller presented an unreadable certificate: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("node: a caller presented a certificate that is not valid now (notBefore=%s notAfter=%s)",
			leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	if !hasPurpose(leaf, x509.ExtKeyUsageClientAuth) {
		return errors.New("node: a caller presented a certificate that is not a client certificate")
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return fmt.Errorf("node: a caller presented a %T key; this node identifies ed25519 identities", leaf.PublicKey)
	}
	if log != nil {
		log.Debug(context.Background(), "node: a caller's certificate was accepted",
			"caller_node_id", NodeIDFor(leaf.PublicKey.(ed25519.PublicKey)))
	}
	return nil
}

// peerTLSConfig is the CLIENT half: reach the peer over TLS, present our own certificate, and
// refuse to talk to a machine whose key is not the one this address is remembered by.
//
// Verification is ours rather than the system's, and that is the point rather than a shortcut: the
// certificate is self-signed and the address is one the operator typed, so a CA chain would fail
// while proving nothing, and a hostname check would fail for every peer configured by IP. What is
// checked instead is the identity the TOFU record holds — the node id derived from the presented
// key — which makes the SECOND conversation as verified as a pin, and the first one only as
// trustworthy as the path that led to it (a mesh that authenticates devices, or loopback).
//
// label and addr are what a FIRST contact files the pin under, and they are passed in rather than
// looked up afterwards because this callback is where the identity is proved: looking it up later
// would mean a contact that proved an identity and then could not say where it happened.
func peerTLSConfig(ourCert tls.Certificate, p *knownPeers, label, addr string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Our own identity, presented so the peer can read the caller out of it. This is the whole
		// of "authentication" on this side: no token travels, and there is nothing here that a
		// listener on the wire could replay. Whether the peer MAY call this one is not something
		// this certificate is checked against — there is no list of accepted callers on either
		// side (node/options.go checkRemoteListen), only the fact of who it is.
		Certificates: []tls.Certificate{ourCert},
		// The system verifier would reject a self-signed certificate before our own check ran.
		// Nothing it does is being skipped in spirit: none of it is the check this node makes.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPeerCertificate(p, label, addr, rawCerts)
		},
	}
}

// verifyPeerCertificate is the whole check: readable, current, and the identity this peer is
// remembered by.
func verifyPeerCertificate(p *knownPeers, label, addr string, rawCerts [][]byte) error {
	who := peerWords(label, addr)
	if len(rawCerts) == 0 {
		return fmt.Errorf("node: %s presented no certificate", who)
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("node: %s presented an unreadable certificate: %w", who, err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fmt.Errorf("node: %s presented a certificate that is not valid now (notBefore=%s notAfter=%s)",
			who, leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("node: %s presented a %T key; this node pins ed25519 identities", who, leaf.PublicKey)
	}

	return p.confirm(label, addr, NodeIDFor(pub))
}
