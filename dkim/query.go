package dkim

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ed25519"
)

type verifier interface {
	Public() crypto.PublicKey
	Verify(hash crypto.Hash, hashed []byte, sig []byte) error
}

type rsaVerifier struct {
	*rsa.PublicKey
}

func (v rsaVerifier) Public() crypto.PublicKey {
	return v.PublicKey
}

func (v rsaVerifier) Verify(hash crypto.Hash, hashed, sig []byte) error {
	return rsa.VerifyPKCS1v15(v.PublicKey, hash, hashed, sig)
}

type ed25519Verifier struct {
	ed25519.PublicKey
}

func (v ed25519Verifier) Public() crypto.PublicKey {
	return v.PublicKey
}

func (v ed25519Verifier) Verify(hash crypto.Hash, hashed, sig []byte) error {
	if !ed25519.Verify(v.PublicKey, hashed, sig) {
		return errors.New("dkim: invalid Ed25519 signature")
	}
	return nil
}

type QueryResult struct {
	Verifier  verifier
	KeyAlgo   string
	HashAlgos []string
	Notes     string
	Services  []string
	Flags     []string
}

// QueryMethod is a DKIM query method.
type QueryMethod string

const (
	// DNS TXT resource record (RR) lookup algorithm
	QueryMethodDNSTXT QueryMethod = "dns/txt"
)

type txtLookupFunc func(domain string) ([]string, error)
type queryFunc func(domain, selector string, txtLookup txtLookupFunc) (*QueryResult, error)

var queryMethods = map[QueryMethod]queryFunc{
	QueryMethodDNSTXT: queryDNSTXT,
}

func queryDNSTXT(domain, selector string, txtLookup txtLookupFunc) (*QueryResult, error) {
	var txts []string
	var err error
	if txtLookup != nil {
		txts, err = txtLookup(selector + "._domainkey." + domain)
	} else {
		txts, err = net.LookupTXT(selector + "._domainkey." + domain)
	}

	if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
		return nil, tempFailError("key unavailable: " + err.Error())
	} else if err != nil {
		return nil, permFailError("no key for signature: " + err.Error())
	}

	// Long keys are split in multiple parts
	txt := strings.Join(txts, "")

	return parsePublicKey(txt)
}

func parsePublicKey(s string) (*QueryResult, error) {
	params, err := parseHeaderParams(s)
	if err != nil {
		return nil, permFailError("key syntax error: " + err.Error())
	}

	res := new(QueryResult)

	if v, ok := params["v"]; ok && v != "DKIM1" {
		return nil, permFailError("incompatible public key version")
	}

	p, ok := params["p"]
	if !ok {
		return nil, permFailError("key syntax error: missing public key data")
	}
	if p == "" {
		return nil, permFailError("key revoked")
	}
	p = strings.ReplaceAll(p, " ", "")
	b, err := base64.StdEncoding.DecodeString(p)
	if err != nil {
		return nil, permFailError("key syntax error: " + err.Error())
	}
	switch params["k"] {
	case "rsa", "":
		pub, err := x509.ParsePKIXPublicKey(b)
		if err != nil {
			return nil, permFailError("key syntax error: " + err.Error())
		}
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, permFailError("key syntax error: not an RSA public key")
		}
		// RFC 8301 section 3.2: verifiers MUST NOT consider signatures using
		// RSA keys of less than 1024 bits as valid signatures.
		if rsaPub.Size()*8 < 1024 {
			return nil, permFailError(fmt.Sprintf("key is too short: want 1024 bits, has %v bits", rsaPub.Size()*8))
		}
		res.Verifier = rsaVerifier{rsaPub}
		res.KeyAlgo = "rsa"
	case "ed25519":
		if len(b) != ed25519.PublicKeySize {
			return nil, permFailError(fmt.Sprintf("invalid Ed25519 public key size: %v bytes", len(b)))
		}
		ed25519Pub := ed25519.PublicKey(b)
		res.Verifier = ed25519Verifier{ed25519Pub}
		res.KeyAlgo = "ed25519"
	default:
		return nil, permFailError("unsupported key algorithm")
	}

	if hashesStr, ok := params["h"]; ok {
		res.HashAlgos = parseTagList(hashesStr)
	}
	if notes, ok := params["n"]; ok {
		res.Notes = notes
	}
	if servicesStr, ok := params["s"]; ok {
		services := parseTagList(servicesStr)

		hasWildcard := false
		for _, s := range services {
			if s == "*" {
				hasWildcard = true
				break
			}
		}
		if !hasWildcard {
			res.Services = services
		}
	}
	if flagsStr, ok := params["t"]; ok {
		res.Flags = parseTagList(flagsStr)
	}

	return res, nil
}
