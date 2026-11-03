// Command testhttp3server is a small HTTP/3 server for trying resonate's HTTP/3
// support without any service of your own. It serves the same handlers over
//
//   - HTTP/3 (QUIC, on UDP) and
//   - HTTPS on TCP (HTTP/1.1, and HTTP/2 where the client offers it),
//
// on the same port number, with a self-signed certificate generated at startup.
// That makes the difference HTTP/3 being opt-in visible: GET /proto reports the
// protocol version the request arrived over, so the same URL says "HTTP/1.1"
// to a client that doesn't ask for HTTP/3 and "HTTP/3.0" to one that does.
//
//	GET  /proto     JSON: the protocol version, TLS version and ALPN of this request
//	POST /echo      echoes the request body
//	GET  /events    a finite Server-Sent Events stream (?n=, ?interval=), for SSE over HTTP/3
//	GET  /healthz   plain "ok"
//
//	go run ./examples/testhttp3server                       # :8443, TCP and UDP
//	go run ./examples/testhttp3server -h3-only              # no TCP listener
//	go run ./examples/testhttp3server -cert-out cert.pem    # also save the certificate
//
// The certificate is self-signed, so resonate needs `--insecure` (`insecure: true`),
// or `--ca-cert cert.pem` (`ca_file`) after -cert-out. It logs each request as it
// arrives and as it finishes; for a performance run use -log-requests=false, which
// leaves a one-line summary every -stats-interval instead.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go/http3"
)

type options struct {
	eventInterval time.Duration
	events        int
	logger        *log.Logger // nil logs nothing
	logRequests   bool
}

func (o options) logf(format string, args ...any) {
	if o.logRequests && o.logger != nil {
		o.logger.Printf(format, args...)
	}
}

func main() {
	addr := flag.String("addr", ":8443", "address to listen on (UDP for HTTP/3 and, unless -h3-only, TCP on the same port)")
	h3Only := flag.Bool("h3-only", false, "serve HTTP/3 only, with no TCP listener (a client that doesn't ask for HTTP/3 then can't connect)")
	certOut := flag.String("cert-out", "", "also write the server certificate (PEM) to this file, to use as resonate's ca_file / --ca-cert")
	interval := flag.Duration("event-interval", 20*time.Millisecond, "gap between /events events")
	events := flag.Int("events", 12, "events GET /events sends before closing it")
	logRequests := flag.Bool("log-requests", true, "log every request (turn off for load tests)")
	statsEvery := flag.Duration("stats-interval", 10*time.Second, "print a one-line request summary this often when there was traffic (0 disables)")
	flag.Parse()

	cert, certPEM, err := selfSignedCert()
	if err != nil {
		log.Fatal(err)
	}
	if *certOut != "" {
		if err := os.WriteFile(*certOut, certPEM, 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote the server certificate to %s", *certOut)
	}

	opts := options{eventInterval: *interval, events: *events, logger: log.Default(), logRequests: *logRequests}
	srv, err := listen(*addr, !*h3Only, cert, opts)
	if err != nil {
		log.Fatal(err)
	}

	if *statsEvery > 0 {
		go func() {
			for range time.Tick(*statsEvery) {
				if line, ok := srv.access.summary(*statsEvery); ok {
					log.Print(line)
				}
			}
		}()
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		sig := <-stop
		n, failed := srv.access.totals()
		log.Printf("received %v: shutting down after %d requests (%d failed)", sig, n, failed)
		srv.close()
	}()

	log.Printf("example HTTP/3 server listening on %s (UDP, HTTP/3)", srv.udpAddr)
	if srv.tcpAddr != "" {
		log.Printf("and on %s (TCP, HTTPS: HTTP/1.1 and HTTP/2), advertising HTTP/3 via Alt-Svc", srv.tcpAddr)
	} else {
		log.Println("TCP listener off (-h3-only): only a client using HTTP/3 can connect")
	}
	log.Println("certificate: self-signed; use --insecure (or --ca-cert with -cert-out)")
	log.Println("endpoints: GET /proto, POST /echo, GET /events, GET /healthz")
	if !*logRequests {
		log.Println("per-request logging is off; a summary line is printed every", *statsEvery, "while there is traffic")
	}
	srv.wait()
}

// servers is the running listeners plus the counters behind the summary line.
type servers struct {
	h3      *http3.Server
	tcp     *http.Server
	udpAddr string
	tcpAddr string
	access  *accessLog
	done    chan struct{}
}

func (s *servers) close() {
	s.h3.Close()
	if s.tcp != nil {
		s.tcp.Close()
	}
}

func (s *servers) wait() { <-s.done }

// listen binds UDP (and TCP, if withTCP) on addr and starts serving. If addr's
// port is 0, the UDP port the OS picks is reused for TCP, retrying if another
// process already holds that TCP port, so both sit on one port number.
func listen(addr string, withTCP bool, cert tls.Certificate, opts options) (*servers, error) {
	var pc net.PacketConn
	var tcpLn net.Listener
	var err error
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 20; attempt++ {
		if pc, err = net.ListenPacket("udp", addr); err != nil {
			return nil, err
		}
		if !withTCP {
			break
		}
		tcpLn, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port)))
		if err == nil {
			break
		}
		pc.Close()
		pc = nil
		if _, port, _ := net.SplitHostPort(addr); port != "0" {
			return nil, err // a fixed port that is taken is an error, not a retry
		}
	}
	if pc == nil {
		return nil, fmt.Errorf("could not bind TCP and UDP to the same port: %w", err)
	}

	port := pc.LocalAddr().(*net.UDPAddr).Port
	var logf func(string, ...any)
	if opts.logger != nil {
		logf = opts.logger.Printf
	}
	access := newAccessLog(logf, opts.logRequests)
	handler := access.wrap(newMux(opts, port))
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}

	s := &servers{access: access, udpAddr: pc.LocalAddr().String(), done: make(chan struct{})}
	s.h3 = &http3.Server{Handler: handler, TLSConfig: http3.ConfigureTLSConfig(tlsCfg)}
	var live sync.WaitGroup
	live.Add(1)
	go func() { defer live.Done(); s.h3.Serve(pc) }()
	if tcpLn != nil {
		s.tcpAddr = tcpLn.Addr().String()
		s.tcp = &http.Server{Handler: handler, TLSConfig: tlsCfg, ReadHeaderTimeout: 5 * time.Second}
		live.Add(1)
		go func() {
			defer live.Done()
			if err := s.tcp.ServeTLS(tcpLn, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Print(err)
			}
		}()
	}
	go func() { live.Wait(); close(s.done) }()
	return s, nil
}

func newMux(opts options, port int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /proto", func(w http.ResponseWriter, r *http.Request) {
		info := map[string]string{"proto": r.Proto, "remote": r.RemoteAddr}
		if r.TLS != nil {
			info["tls"] = tls.VersionName(r.TLS.Version)
			info["alpn"] = r.TLS.NegotiatedProtocol
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		io.Copy(w, io.LimitReader(r.Body, 1<<20))
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) { handleEvents(opts, w, r) })

	// Advertise HTTP/3 on responses sent over TCP, as a real deployment would. resonate
	// doesn't act on it (HTTP/3 is opt-in), but a browser or curl would.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 3 {
			w.Header().Set("Alt-Svc", fmt.Sprintf(`h3=":%d"; ma=3600`, port))
		}
		mux.ServeHTTP(w, r)
	})
}

// handleEvents sends a finite event stream, then closes it.
func handleEvents(opts options, w http.ResponseWriter, r *http.Request) {
	n, interval := opts.events, opts.eventInterval
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 10000 {
		n = v
	}
	if v, err := time.ParseDuration(r.URL.Query().Get("interval")); err == nil && v > 0 && v <= 10*time.Second {
		interval = v
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	if f != nil {
		f.Flush()
	}
	opts.logf("events: stream opened over %s, %d events %s apart", r.Proto, n, interval)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for sent := 0; sent < n; {
		select {
		case <-r.Context().Done():
			opts.logf("events: client went away after %d of %d events", sent, n)
			return
		case <-tick.C:
			if _, err := fmt.Fprintf(w, "id: %d\nevent: tick\ndata: {\"n\":%d,\"proto\":%q}\n\n", sent, sent, r.Proto); err != nil {
				return
			}
			if f != nil {
				f.Flush()
			}
			sent++
		}
	}
	opts.logf("events: sent all %d events, closing the stream", n)
}

// selfSignedCert makes a throwaway ECDSA certificate for localhost, 127.0.0.1 and ::1.
func selfSignedCert() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "resonate example HTTP/3 server"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-signed, so it can be handed to ca_file as its own root
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	return cert, certPEM, err
}
