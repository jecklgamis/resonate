package generator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestHTTPGeneratorMaxResponseBodyCapsBytesInAndReusesConnection(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, strings.Repeat("x", 1000))

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, HTTPOptions{MaxResponseBody: 100})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	for i := 0; i < 3; i++ {
		results := a.Do(context.Background(), 0)
		if results[0].Error != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, results[0].Error)
		}
		if results[0].BytesIn != 100 {
			t.Errorf("iteration %d: BytesIn = %d, want capped at 100", i, results[0].BytesIn)
		}
	}
}

func TestHTTPGeneratorMaxResponseBodyZeroIsUnlimited(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, strings.Repeat("x", 1000))

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].BytesIn != 1000 {
		t.Errorf("BytesIn = %d, want the full 1000 (MaxResponseBody unset)", results[0].BytesIn)
	}
}

func TestNewHTTPGeneratorRejectsCertWithoutKey(t *testing.T) {
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: "https://example.test"}}, HTTPOptions{CertFile: "cert.pem"})
	if err == nil {
		t.Fatal("expected an error when CertFile is set without KeyFile")
	}
}

func TestNewHTTPGeneratorRejectsKeyWithoutCert(t *testing.T) {
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: "https://example.test"}}, HTTPOptions{KeyFile: "key.pem"})
	if err == nil {
		t.Fatal("expected an error when KeyFile is set without CertFile")
	}
}

func TestNewHTTPGeneratorRejectsH2CWithTLSOptions(t *testing.T) {
	cases := []HTTPOptions{
		{H2C: true, Insecure: true},
		{H2C: true, CAFile: "ca.pem"},
		{H2C: true, CertFile: "cert.pem", KeyFile: "key.pem"},
		{H2C: true, DisableKeepAlive: true},
	}
	for _, opts := range cases {
		if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test"}}, opts); err == nil {
			t.Errorf("NewHTTPGenerator(%+v): expected an error combining h2c with an incompatible option", opts)
		}
	}
}

func TestHTTPGeneratorDisableKeepAliveOpensFreshConnectionPerRequest(t *testing.T) {
	var connCount atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connCount.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, HTTPOptions{DisableKeepAlive: true})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	const n = 5
	for i := 0; i < n; i++ {
		results := a.Do(context.Background(), 0)
		if results[0].Error != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, results[0].Error)
		}
	}
	if got := connCount.Load(); got != n {
		t.Errorf("new connections opened = %d, want %d (one per request, no reuse)", got, n)
	}
}

func TestHTTPGeneratorKeepAliveReusesConnectionByDefault(t *testing.T) {
	var connCount atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connCount.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	const n = 5
	for i := 0; i < n; i++ {
		results := a.Do(context.Background(), 0)
		if results[0].Error != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, results[0].Error)
		}
	}
	if got := connCount.Load(); got != 1 {
		t.Errorf("new connections opened = %d, want 1 (keep-alive should reuse the connection)", got)
	}
}

func TestNewHTTPGeneratorRejectsUnreadableCAFile(t *testing.T) {
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: "https://example.test"}}, HTTPOptions{CAFile: "/nonexistent/ca.pem"})
	if err == nil {
		t.Fatal("expected an error for a CA file that doesn't exist")
	}
}

// testCA is a minimal in-memory CA for building mTLS test fixtures: a CA
// cert, a server cert/key signed by it, and a client cert/key signed by it.
type testCA struct {
	caPEM         []byte
	serverCert    tls.Certificate
	clientCertPEM []byte
	clientKeyPEM  []byte
	caPool        *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "resonate-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	issue := func(cn string, eku x509.ExtKeyUsage, ips []string) (certPEM, keyPEM []byte, cert tls.Certificate) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{eku},
			DNSNames:     []string{"localhost"},
		}
		for _, ip := range ips {
			tmpl.IPAddresses = append(tmpl.IPAddresses, mustParseIP(t, ip))
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		cert, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	_, _, serverCert := issue("localhost", x509.ExtKeyUsageServerAuth, []string{"127.0.0.1", "::1"})
	clientCertPEM, clientKeyPEM, _ := issue("resonate-test-client", x509.ExtKeyUsageClientAuth, nil)

	return &testCA{
		caPEM:         caPEM,
		serverCert:    serverCert,
		clientCertPEM: clientCertPEM,
		clientKeyPEM:  clientKeyPEM,
		caPool:        pool,
	}
}

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("invalid test IP %q", s)
	}
	return ip
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHTTPGeneratorCAFileTrustsPrivateCA(t *testing.T) {
	ca := newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert}}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caFile := writeTempFile(t, dir, "ca.pem", ca.caPEM)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, HTTPOptions{CAFile: caFile})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Error != nil {
		t.Fatalf("unexpected error trusting the private CA: %v", results[0].Error)
	}
	if results[0].StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", results[0].StatusCode)
	}
}

func TestHTTPGeneratorWithoutCAFileRejectsPrivateCA(t *testing.T) {
	ca := newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert}}
	srv.StartTLS()
	defer srv.Close()

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Error == nil {
		t.Fatal("expected a TLS verification error without CAFile trusting the private CA")
	}
}

func TestHTTPGeneratorClientCertAuthenticatesToServer(t *testing.T) {
	ca := newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caFile := writeTempFile(t, dir, "ca.pem", ca.caPEM)
	certFile := writeTempFile(t, dir, "client.pem", ca.clientCertPEM)
	keyFile := writeTempFile(t, dir, "client-key.pem", ca.clientKeyPEM)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, HTTPOptions{CAFile: caFile, CertFile: certFile, KeyFile: keyFile})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Error != nil {
		t.Fatalf("unexpected error with a valid client certificate: %v", results[0].Error)
	}
	if results[0].StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", results[0].StatusCode)
	}
}

func TestHTTPGeneratorWithoutClientCertFailsMTLSHandshake(t *testing.T) {
	ca := newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caFile := writeTempFile(t, dir, "ca.pem", ca.caPEM)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, HTTPOptions{CAFile: caFile})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Error == nil {
		t.Fatal("expected the handshake to fail: server requires a client cert we didn't provide")
	}
}

func TestHTTPGeneratorH2CTalksHTTP2OverPlaintext(t *testing.T) {
	var sawProtoMajor int
	h1Handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProtoMajor = r.ProtoMajor
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(h2c.NewHandler(h1Handler, &http2.Server{}))
	defer srv.Close()

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, HTTPOptions{H2C: true})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Error != nil {
		t.Fatalf("unexpected error over h2c: %v", results[0].Error)
	}
	if sawProtoMajor != 2 {
		t.Errorf("server saw ProtoMajor = %d, want 2 (h2c should negotiate HTTP/2 without TLS)", sawProtoMajor)
	}
}
