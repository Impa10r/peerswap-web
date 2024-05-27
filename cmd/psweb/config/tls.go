package config

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// generates Certificate Autonrity CA.srt
func GenerateCA() error {
	crtPath := filepath.Join(Config.DataDir, "CA.crt")
	keyPath := filepath.Join(Config.DataDir, "CA.key")

	// Generate RSA private key
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// Save private key to file
	privKeyFile, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer privKeyFile.Close()
	pem.Encode(privKeyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)})

	// Create a certificate signing request (CSR)
	csrTemplate := x509.CertificateRequest{
		Subject: pkix.Name{
			Organization: []string{"PeerSwap Web UI"},
		},
		SignatureAlgorithm: x509.SHA256WithRSA,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &csrTemplate, privKey)
	if err != nil {
		return err
	}

	// Parse the CSR
	certFromCSR, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return err
	}
	if err := certFromCSR.CheckSignature(); err != nil {
		return err
	}

	// Create a certificate based on the CSR
	certTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               certFromCSR.Subject,
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0), // Valid for 10 years
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &certTemplate, &certTemplate, certFromCSR.PublicKey.(crypto.PublicKey), privKey)
	if err != nil {
		return err
	}

	// Save the signed certificate to file
	signedCertFile, err := os.Create(crtPath)
	if err != nil {
		return err
	}
	defer signedCertFile.Close()
	pem.Encode(signedCertFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return nil
}

func GenereateServerCertificate() error {
	crtPath := filepath.Join(Config.DataDir, "server.crt")
	keyPath := filepath.Join(Config.DataDir, "server.key")
	crtPathCA := filepath.Join(Config.DataDir, "CA.crt")
	keyPathCA := filepath.Join(Config.DataDir, "CA.key")

	IPs := strings.Split(Config.ServerIPs, " ")

	// Generate RSA private key for the server
	serverPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// Save server private key to file
	serverPrivKeyFile, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer serverPrivKeyFile.Close()
	pem.Encode(serverPrivKeyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverPrivKey)})

	// Get the hostname of the machine
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	// Set certificate name
	subject := pkix.Name{
		Organization: []string{"PeerSwap Web UI"},
	}

	// Add alternative IP addresses
	var ipAdresses []net.IP
	for _, ip := range IPs {
		ipAdresses = append(ipAdresses, net.ParseIP(ip))
	}

	// Load the CA private key
	caPrivKeyPEM, err := os.ReadFile(keyPathCA)
	if err != nil {
		return err
	}
	caPrivKeyBlock, _ := pem.Decode(caPrivKeyPEM)
	caPrivKey, err := x509.ParsePKCS1PrivateKey(caPrivKeyBlock.Bytes)
	if err != nil {
		return err
	}

	// Load the CA certificate
	caCertPEM, err := os.ReadFile(crtPathCA)
	if err != nil {
		return err
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return err
	}

	// Create the server certificate template
	serverCertTemplate := x509.Certificate{
		SerialNumber:       big.NewInt(1),
		Subject:            subject,
		SignatureAlgorithm: x509.SHA256WithRSA,
		NotBefore:          time.Now(),
		NotAfter:           time.Now().AddDate(10, 0, 0), // Valid for 10 years
		DNSNames: []string{
			"localhost",
			hostname + ".local"},
		IPAddresses: ipAdresses,
	}

	// Sign the server certificate with the CA's private key
	serverCertDER, err := x509.CreateCertificate(rand.Reader, &serverCertTemplate, caCert, &serverPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return err
	}

	// Save the signed server certificate to file
	serverCertFile, err := os.Create(crtPath)
	if err != nil {
		return err
	}
	defer serverCertFile.Close()
	pem.Encode(serverCertFile, &pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})

	Save()

	return nil
}

func GenerateClientCertificate(password string) error {
	crtPathCA := filepath.Join(Config.DataDir, "CA.crt")
	keyPathCA := filepath.Join(Config.DataDir, "CA.key")

	// Generate RSA private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// Load CA certificate
	caCertPEM, err := os.ReadFile(crtPathCA)
	if err != nil {
		return err
	}

	// Load CA key
	caKeyPEM, err := os.ReadFile(keyPathCA)
	if err != nil {
		return err
	}

	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		return errors.New("pem.Decode(caCertPEM)")
	}

	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return err
	}

	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		fmt.Printf("Failed to parse CA private key PEM\n")
		return err
	}

	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return err
	}

	certTemplate := x509.Certificate{
		SerialNumber: big.NewInt(1234567890),
		Subject: pkix.Name{
			Organization: []string{"PSWeb Client"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().AddDate(10, 0, 0), // Valid for 10 years

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	// Create client certificate
	clientCertBytes, err := x509.CreateCertificate(rand.Reader, &certTemplate, caCert, &privateKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	// Export the certificate and key to PKCS#12
	p12Data, err := pkcs12.Modern.Encode(privateKey, &x509.Certificate{
		Raw: clientCertBytes,
	}, []*x509.Certificate{caCert}, password)
	if err != nil {
		return err
	}

	err = os.WriteFile(filepath.Join(Config.DataDir, "client.p12"), p12Data, 0644)
	if err != nil {
		return err
	}

	return nil
}

const charset = "abcdefghijkmnopqrstuvwxyzABCDEFGHIJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword generates a random password of a given length
func GeneratePassword(length int) (string, error) {
	password := make([]byte, length)
	charsetLength := big.NewInt(int64(len(charset)))

	for i := range password {
		randomIndex, err := rand.Int(rand.Reader, charsetLength)
		if err != nil {
			return "", err
		}
		password[i] = charset[randomIndex.Int64()]
	}

	return string(password), nil
}
