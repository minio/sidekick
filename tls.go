// Copyright (c) 2021-2024 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// generateTLSCertKey creates valid key/cert with registered DNS or IP address
// depending on the passed parameter. That way, we can use tls config without
// passing InsecureSkipVerify flag.  This code is a simplified version of
// https://golang.org/src/crypto/tls/generate_cert.go
func generateTLSCertKey(host string) ([]byte, []byte, error) {
	validFor := 365 * 24 * time.Hour
	if len(host) == 0 {
		return nil, nil, fmt.Errorf("missing host parameter")
	}

	publicKey := func(priv interface{}) interface{} {
		switch k := priv.(type) {
		case *rsa.PrivateKey:
			return &k.PublicKey
		case *ecdsa.PrivateKey:
			return &k.PublicKey
		default:
			return nil
		}
	}

	pemBlockForKey := func(priv interface{}) *pem.Block {
		switch k := priv.(type) {
		case *rsa.PrivateKey:
			return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
		case *ecdsa.PrivateKey:
			b, err := x509.MarshalECPrivateKey(k)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Unable to marshal ECDSA private key: %v", err)
				os.Exit(2)
			}
			return &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
		default:
			return nil
		}
	}

	var priv interface{}
	var err error
	priv, err = ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}
	notBefore := time.Now()
	notAfter := notBefore.Add(validFor)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := crand.Int(crand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	hosts := strings.Split(host, ",")
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(crand.Reader, &template, &template, publicKey(priv), priv)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	certOut := bytes.NewBuffer([]byte{})
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyOut := bytes.NewBuffer([]byte{})
	pem.Encode(keyOut, pemBlockForKey(priv))

	return certOut.Bytes(), keyOut.Bytes(), nil
}

// LoadX509KeyPair - load an X509 key pair (private key , certificate)
// from the provided paths. The private key may be encrypted and is
// decrypted using the flags: --key-password.
//
//nolint:staticcheck
func LoadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEMBlock, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("unable to read the public key: %w", err)
	}
	keyPEMBlock, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("unable to read the private key: %w", err)
	}
	key, rest := pem.Decode(keyPEMBlock)
	if len(rest) > 0 {
		return tls.Certificate{}, errors.New("the private key contains additional data")
	}
	if key == nil {
		return tls.Certificate{}, errors.New("the private key is not readable")
	}
	if x509.IsEncryptedPEMBlock(key) {
		password := globalKeyPassword
		if len(password) == 0 {
			return tls.Certificate{}, errors.New("the private key is encrypted, please set the flag --key-password")
		}
		decryptedKey, decErr := x509.DecryptPEMBlock(key, []byte(password))
		if decErr != nil {
			return tls.Certificate{}, fmt.Errorf("unable to decrypt private key: %w", decErr)
		}
		keyPEMBlock = pem.EncodeToMemory(&pem.Block{Type: key.Type, Bytes: decryptedKey})
	}
	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("unable to load certs: %w", err)
	}
	return cert, nil
}
