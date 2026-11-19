package mail

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// received is one message that the fake server accepted.
type received struct {
	from, data       string
	to               []string
	authUser, authPw string
	tls              bool
}

// fakeServer is a minimal in-process SMTP server for the tests. It speaks
// just enough of RFC 5321 for go-mail: greeting, EHLO, STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA, RSET, NOOP and QUIT.
type fakeServer struct {
	ln       net.Listener
	tlsCfg   *tls.Config
	pool     *x509.CertPool
	got      []received
	mode     string // "plain", "starttls" or "tls"
	authPw   string // the password that AUTH accepts; empty: AUTH is not offered
	authUser string
	pings    int
	mu       sync.Mutex
	// stall makes the server accept the connection and then say nothing.
	stall bool
	// noStartTLS makes a "starttls" server not offer the extension.
	noStartTLS bool
	// rejectAuth makes AUTH fail with 535.
	rejectAuth bool
	// rcptReply, when set, answers every RCPT TO with this line; "%s" is the
	// address of the command (a server that echoes the recipient).
	rcptReply string
}

func newCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// startFakeServer listens on a free loopback port. mode is "plain",
// "starttls" or "tls".
func startFakeServer(t *testing.T, mode string, configure func(*fakeServer)) *fakeServer {
	t.Helper()

	cert, pool := newCert(t)
	s := &fakeServer{mode: mode, pool: pool, tlsCfg: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}

	if configure != nil {
		configure(s)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s.ln = ln
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go s.serve(conn)
		}
	}()

	return s
}

func (s *fakeServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// clientTLS is the TLS configuration of a client that trusts the server.
func (s *fakeServer) clientTLS() *tls.Config {
	return &tls.Config{RootCAs: s.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

func (s *fakeServer) messages() []received {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]received(nil), s.got...)
}

func (s *fakeServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pings
}

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close()

	s.mu.Lock()
	s.pings++
	s.mu.Unlock()

	if s.stall {
		time.Sleep(10 * time.Second)

		return
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	encrypted := false

	if s.mode == "tls" {
		tc := tls.Server(conn, s.tlsCfg)
		if err := tc.Handshake(); err != nil {
			return
		}

		conn, encrypted = tc, true
	}

	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	write("220 fake ESMTP")

	var cur received

	cur.tls = encrypted

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			write("250-fake")

			if s.mode == "starttls" && !encrypted && !s.noStartTLS {
				write("250-STARTTLS")
			}

			if s.authPw != "" || s.rejectAuth {
				write("250-AUTH PLAIN")
			}

			write("250 8BITMIME")
		case verb == "STARTTLS":
			write("220 ready")

			tc := tls.Server(conn, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}

			conn, r, encrypted = tc, bufio.NewReader(tc), true
			cur.tls = true
			write = func(line string) { _, _ = tc.Write([]byte(line + "\r\n")) }
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			parts := strings.Fields(line)
			if s.rejectAuth || len(parts) < 3 {
				write("535 5.7.8 authentication failed")

				continue
			}

			raw, _ := base64.StdEncoding.DecodeString(parts[2])
			fields := strings.Split(string(raw), "\x00")

			if len(fields) != 3 || fields[2] != s.authPw || (s.authUser != "" && fields[1] != s.authUser) {
				write("535 5.7.8 authentication failed")

				continue
			}

			cur.authUser, cur.authPw = fields[1], fields[2]

			write("235 2.7.0 ok")
		case strings.HasPrefix(verb, "MAIL FROM:"):
			cur.from = strings.TrimSpace(line[len("MAIL FROM:"):])

			write("250 ok")
		case strings.HasPrefix(verb, "RCPT TO:"):
			addr := strings.TrimSpace(line[len("RCPT TO:"):])
			if s.rcptReply != "" {
				write(strings.ReplaceAll(s.rcptReply, "%s", addr))

				continue
			}

			cur.to = append(cur.to, addr)

			write("250 ok")
		case verb == "DATA":
			write("354 go ahead")

			var data strings.Builder

			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}

				if l == ".\r\n" {
					break
				}

				data.WriteString(l)
			}

			cur.data = data.String()

			s.mu.Lock()
			s.got = append(s.got, cur)
			s.mu.Unlock()

			cur = received{tls: encrypted, authUser: cur.authUser, authPw: cur.authPw}

			write("250 queued")
		case verb == "RSET", verb == "NOOP":
			write("250 ok")
		case verb == "QUIT":
			write("221 bye")

			return
		default:
			write("502 not implemented")
		}
	}
}
