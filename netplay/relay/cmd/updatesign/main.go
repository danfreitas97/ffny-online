// Command updatesign makes and checks the signature on the launcher's update
// manifest (latest.json). The launcher verifies it with the public key built
// into it (src/hooks/updater.c), so a manifest -- and through its SHA-256 the
// setup it names -- is only accepted from whoever holds the private key.
//
//	updatesign keygen -key <file>                 new ECDSA P-256 key; prints the public key
//	updatesign sign   -key <file> -in latest.json -out latest.json.sig
//	updatesign verify -pub <hex>  -in latest.json -sig latest.json.sig
//
// The signature file is 128 hex characters: r and s, 32 bytes each, big-endian,
// over SHA-256 of the manifest's exact bytes -- the form Windows' BCrypt checks.
// The public key is printed the same way: X and Y, 32 bytes each.
// Keep the private key off the repository and off the server.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	key := fs.String("key", "", "private key file (PEM)")
	in := fs.String("in", "", "manifest to sign or verify")
	out := fs.String("out", "", "signature file to write")
	sig := fs.String("sig", "", "signature file to check")
	pub := fs.String("pub", "", "public key, 128 hex characters")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(*key)
	case "sign":
		err = sign(*key, *in, *out)
	case "verify":
		err = verify(*pub, *in, *sig)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "updatesign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: updatesign keygen|sign|verify (see the source)")
	os.Exit(2)
}

func fixed32(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func publicHex(k *ecdsa.PublicKey) string {
	return hex.EncodeToString(append(fixed32(k.X), fixed32(k.Y)...))
}

func keygen(path string) error {
	if path == "" {
		return errors.New("-key is required")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists; refusing to replace a signing key", path)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return err
	}
	fmt.Println(publicHex(&k.PublicKey))
	return nil
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("not a PEM key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("not an ECDSA P-256 key")
	}
	return k, nil
}

func sign(keyPath, in, out string) error {
	if keyPath == "" || in == "" || out == "" {
		return errors.New("-key, -in and -out are required")
	}
	k, err := loadKey(keyPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(hex.EncodeToString(append(fixed32(r), fixed32(s)...))), 0o644); err != nil {
		return err
	}
	fmt.Println("public key:", publicHex(&k.PublicKey))
	return nil
}

func verify(pubHex, in, sigPath string) error {
	pb, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pb) != 64 {
		return errors.New("-pub must be 128 hex characters")
	}
	raw, err := os.ReadFile(sigPath)
	if err != nil {
		return err
	}
	sb, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(sb) != 64 {
		return errors.New("the signature must be 128 hex characters")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	k := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(pb[:32]), Y: new(big.Int).SetBytes(pb[32:])}
	digest := sha256.Sum256(data)
	if !ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sb[:32]), new(big.Int).SetBytes(sb[32:])) {
		return errors.New("signature does NOT match")
	}
	fmt.Println("signature OK")
	return nil
}
