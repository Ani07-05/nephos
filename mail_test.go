package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestSMTPMessageIsWellFormed(t *testing.T) {
	s, err := NewSMTPMailer("smtp.example.com:587", "u", "p", "nephos <hello@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.build(inviteMail("bob@example.com", "counter", "https://counter.nephos.example.com/", "https://counter.nephos.example.com/_nephos/join?t=abc"))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("To") != "<bob@example.com>" || msg.Header.Get("Message-ID") == "" || msg.Header.Get("Date") == "" {
		t.Fatalf("headers: %v", msg.Header)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q %v", mt, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p) // multipart.Reader already undoes quoted-printable
		if !strings.Contains(string(body), "_nephos/join?t=abc") {
			t.Fatalf("%s part lost the link: %q", p.Header.Get("Content-Type"), body)
		}
		types = append(types, p.Header.Get("Content-Type"))
	}
	if len(types) != 2 || !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Fatalf("part order %v", types)
	}
}

func TestSMTPRefusesToSendCredentialsInClear(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("220 fake ESMTP\r\n"))
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				conn.Write([]byte("250-fake\r\n250 AUTH PLAIN\r\n"))
			case strings.HasPrefix(line, "QUIT"):
				conn.Write([]byte("221 bye\r\n"))
				return
			}
		}
	}()

	s, err := NewSMTPMailer(ln.Addr().String(), "u", "p", "hello@example.com")
	if err != nil {
		t.Fatal(err)
	}
	err = s.Send(context.Background(), loginMail("bob@example.com", "counter", "https://x/y?t=1"))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err=%v", err)
	}
}

type delivery struct {
	from, to, auth, data string
	tlsAtAuth            bool
}

// fakeSMTP is a small TLS-capable SMTP server: STARTTLS or implicit TLS, AUTH PLAIN, one message.
type fakeSMTP struct {
	addr     string
	cert     tls.Certificate
	pool     *x509.CertPool
	wantPass string
	got      chan delivery
}

func newFakeSMTP(t *testing.T, implicit bool, wantPass string) *fakeSMTP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	f := &fakeSMTP{cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool: pool, wantPass: wantPass, got: make(chan delivery, 1)}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.addr = ln.Addr().String()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		f.serve(conn, implicit)
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn, implicit bool) {
	defer conn.Close()
	cfg := &tls.Config{Certificates: []tls.Certificate{f.cert}}
	secure := implicit
	if implicit {
		conn = tls.Server(conn, cfg)
	}
	r := bufio.NewReader(conn)
	say := func(s string) { conn.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	var d delivery
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "EHLO"):
			if secure {
				say("250-fake\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 STARTTLS")
			}
		case line == "STARTTLS":
			say("220 go ahead")
			conn = tls.Server(conn, cfg)
			r = bufio.NewReader(conn)
			secure = true
		case strings.HasPrefix(line, "AUTH PLAIN "):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			d.auth, d.tlsAtAuth = strings.ReplaceAll(string(raw), "\x00", "|"), secure
			if !strings.HasSuffix(d.auth, "|"+f.wantPass) {
				say("535 5.7.8 bad credentials")
				return
			}
			say("235 ok")
		case strings.HasPrefix(line, "MAIL FROM:"):
			d.from = strings.Trim(strings.TrimPrefix(line, "MAIL FROM:"), "<>")
			say("250 ok")
		case strings.HasPrefix(line, "RCPT TO:"):
			d.to = strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>")
			say("250 ok")
		case line == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			d.data = b.String()
			f.got <- d
			say("250 queued")
		case line == "QUIT":
			say("221 bye")
			return
		}
	}
}

func (f *fakeSMTP) mailer(t *testing.T, implicit bool) *smtpMailer {
	t.Helper()
	m, err := NewSMTPMailer(f.addr, "hello@example.com", "app-token", "nephos <hello@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	m.implicitTLS = implicit
	m.tlsConfig = &tls.Config{RootCAs: f.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	return m
}

func TestSMTPDeliversOverSTARTTLSAndImplicitTLS(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		name := map[bool]string{false: "STARTTLS (587)", true: "implicit TLS (465)"}[implicit]
		t.Run(name, func(t *testing.T) {
			f := newFakeSMTP(t, implicit, "app-token")
			link := "https://counter.nephos.example.com/_nephos/join?t=abc123"
			if err := f.mailer(t, implicit).Send(context.Background(), inviteMail("bob@example.com", "counter", "https://counter.nephos.example.com/", link)); err != nil {
				t.Fatal(err)
			}
			d := <-f.got
			if !d.tlsAtAuth {
				t.Fatal("credentials were sent before TLS")
			}
			if d.auth != "|hello@example.com|app-token" || d.from != "hello@example.com" || d.to != "bob@example.com" {
				t.Fatalf("envelope %+v", d)
			}
			if !strings.Contains(d.data, "Subject: counter was shared with you") || !strings.Contains(d.data, "multipart/alternative") {
				t.Fatalf("message headers: %q", d.data[:min(len(d.data), 400)])
			}
		})
	}
}

func TestSMTPWrongPasswordIsAnError(t *testing.T) {
	f := newFakeSMTP(t, false, "right-password")
	err := f.mailer(t, false).Send(context.Background(), loginMail("bob@example.com", "counter", "https://x/y?t=1"))
	if err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("err=%v", err)
	}
}

func TestSMTPRejectsUntrustedCertificate(t *testing.T) {
	f := newFakeSMTP(t, false, "app-token")
	m := f.mailer(t, false)
	m.tlsConfig = &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12} // system roots only
	if err := m.Send(context.Background(), loginMail("bob@example.com", "counter", "https://x/y?t=1")); err == nil {
		t.Fatal("sent to a server with an untrusted certificate")
	}
}
