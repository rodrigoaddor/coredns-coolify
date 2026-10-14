package coolify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestConnectIP(t *testing.T) {
	// The certificate is valid only for the URL hostname, not the dialed IP.
	const hostname = "cloud.example.invalid"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", useTLS), func(t *testing.T) {
			var authority string
			api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != authority {
					t.Errorf("unexpected Host header: %s", r.Host)
				}
				if useTLS && (r.TLS == nil || r.TLS.ServerName != hostname) {
					t.Error("TLS server name was not preserved")
				}
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing authentication")
				}
				if r.URL.Path != "/api/v1/servers/one/domains" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				fmt.Fprint(w, `[{"ip":"192.0.2.1","domains":["app.example.com"]}]`)
			}))
			api.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
			if useTLS {
				api.StartTLS()
			} else {
				api.Start()
			}
			defer api.Close()
			u, _ := url.Parse(api.URL)
			authority = net.JoinHostPort(hostname, u.Port())
			origin := u.Scheme + "://" + authority
			c, err := parse(controller(fmt.Sprintf("coolify %s {\n token secret\n connect_ip 127.0.0.1\n server one\n}", origin)))
			if err != nil {
				t.Fatal(err)
			}
			defer c.client.CloseIdleConnections()
			transport := c.client.Transport.(*http.Transport)
			// A direct IP pin must also work in deployments with proxy environment variables.
			if transport.Proxy != nil {
				t.Fatal("connect_ip must bypass environment proxies")
			}
			if useTLS {
				transport.TLSClientConfig = &tls.Config{RootCAs: roots}
			}
			if err := c.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.snapshot["app.example.com."]) != 1 {
				t.Fatal("did not discover records")
			}
			if useTLS {
				// A different URL hostname must still fail certificate validation.
				c.baseURL = u.Scheme + "://" + net.JoinHostPort("wrong.example.invalid", u.Port())
				if err := c.refresh(context.Background()); err == nil {
					t.Fatal("accepted mismatched TLS certificate")
				}
			}
		})
	}
}

func TestConnectIPConfiguration(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "2001:db8::1"} {
		c, err := parse(controller("coolify https://cloud.example.com {\n token secret\n server one\n connect_ip " + value + "\n}"))
		if err != nil {
			t.Fatalf("valid IP %s: %v", value, err)
		}
		c.client.CloseIdleConnections()
	}
	for _, value := range []string{"", "edge.example.com", "127.0.0.1:443", "[::1]", "fe80::1%eth0", "127.0.0.1 extra", "127.0.0.1\n connect_ip 127.0.0.2"} {
		if _, err := parse(controller("coolify https://cloud.example.com {\n token secret\n server one\n connect_ip " + value + "\n}")); err == nil {
			t.Fatalf("accepted invalid connect_ip %q", value)
		}
	}
}
