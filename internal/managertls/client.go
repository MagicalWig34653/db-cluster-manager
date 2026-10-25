// SPDX-License-Identifier: AGPL-3.0-only

package managertls

import (
	"context"
	"crypto"
	"crypto/tls"
	"errors"
	"net/http"

	"golang.org/x/crypto/acme"
)

// Client is the RFC 8555 boundary; tests may replace it without a CA or network.
type Client interface {
	Register(context.Context, string) error
	Order(context.Context, []string) (*acme.Order, error)
	Authorization(context.Context, string) (*acme.Authorization, error)
	HTTPResponse(string) (string, error)
	ALPNCertificate(string, string) (tls.Certificate, error)
	Accept(context.Context, *acme.Challenge) error
	WaitAuthorization(context.Context, string) error
	Finalize(context.Context, string, []byte) ([][]byte, error)
}

// Factory binds a persisted account key to a CA client.
type Factory func(directory string, key crypto.Signer) Client

// NewClient uses the existing x/crypto RFC 8555 implementation. Unlike
// autocert.Manager it has no internal, uninterruptible renewal goroutines.
func NewClient(directory string, key crypto.Signer) Client {
	return &protocolClient{c: &acme.Client{DirectoryURL: directory, Key: key, HTTPClient: &http.Client{Timeout: requestTimeout}}}
}

type protocolClient struct{ c *acme.Client }

func (p *protocolClient) Register(ctx context.Context, email string) error {
	_, err := p.c.Register(ctx, &acme.Account{Contact: []string{"mailto:" + email}}, acme.AcceptTOS)
	if errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil
	}
	return err
}
func (p *protocolClient) Order(ctx context.Context, domains []string) (*acme.Order, error) {
	return p.c.AuthorizeOrder(ctx, acme.DomainIDs(domains...))
}
func (p *protocolClient) Authorization(ctx context.Context, uri string) (*acme.Authorization, error) {
	return p.c.GetAuthorization(ctx, uri)
}
func (p *protocolClient) HTTPResponse(token string) (string, error) {
	return p.c.HTTP01ChallengeResponse(token)
}
func (p *protocolClient) ALPNCertificate(token, domain string) (tls.Certificate, error) {
	return p.c.TLSALPN01ChallengeCert(token, domain)
}
func (p *protocolClient) Accept(ctx context.Context, challenge *acme.Challenge) error {
	_, err := p.c.Accept(ctx, challenge)
	return err
}
func (p *protocolClient) WaitAuthorization(ctx context.Context, uri string) error {
	_, err := p.c.WaitAuthorization(ctx, uri)
	return err
}
func (p *protocolClient) Finalize(ctx context.Context, uri string, csr []byte) ([][]byte, error) {
	der, _, err := p.c.CreateOrderCert(ctx, uri, csr, true)
	return der, err
}
