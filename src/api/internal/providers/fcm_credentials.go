package providers

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/mail"
	"regexp"
	"strings"
)

// Only these fields reach the Google auth library. Operator uploads cannot
// redirect OAuth exchanges through extra credential fields or universe domains.
type ServiceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	Email        string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id,omitempty"`
	TokenURI     string `json:"token_uri"`
}

var firebaseProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

func ParseServiceAccount(raw []byte, project string) (ServiceAccount, error) {
	var c ServiceAccount
	if len(raw) > 64<<10 || json.Unmarshal(raw, &c) != nil || c.Type != "service_account" || c.ProjectID != project || !firebaseProject.MatchString(project) || c.TokenURI != "https://oauth2.googleapis.com/token" {
		return c, ErrCredentials
	}
	address, err := mail.ParseAddress(c.Email)
	if err != nil || address.Address != c.Email || !strings.HasSuffix(c.Email, ".iam.gserviceaccount.com") {
		return c, ErrCredentials
	}
	block, rest := pem.Decode([]byte(c.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" || strings.TrimSpace(string(rest)) != "" {
		return c, ErrCredentials
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return c, ErrCredentials
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok || key.N.BitLen() < 2048 || key.Validate() != nil {
		return c, ErrCredentials
	}
	return c, nil
}
