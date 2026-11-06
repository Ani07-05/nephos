package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"time"
)

type Mail struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer sends a message. Delivers reports whether it reaches a real inbox, so
// callers can tell the owner to pass links along by hand when it doesn't.
type Mailer interface {
	Send(ctx context.Context, m Mail) error
	Delivers() bool
}

// NewMailer returns the SMTP adapter when addr is set, otherwise the log adapter.
// Credentials come from NEPHOS_SMTP_USER and NEPHOS_SMTP_PASS.
func NewMailer(addr, from string) (Mailer, error) {
	if addr == "" {
		return logMailer{}, nil
	}
	return NewSMTPMailer(addr, os.Getenv("NEPHOS_SMTP_USER"), os.Getenv("NEPHOS_SMTP_PASS"), from)
}

// logMailer is the development adapter: it prints the message instead of sending it.
type logMailer struct{}

func (logMailer) Delivers() bool { return false }

func (logMailer) Send(_ context.Context, m Mail) error {
	log.Printf("mail to %s: %s\n%s", m.To, m.Subject, m.Text)
	return nil
}

type smtpMailer struct {
	addr, host, user, pass string
	from                   mail.Address
	timeout                time.Duration
	implicitTLS            bool        // port 465 style: TLS from the first byte
	tlsConfig              *tls.Config // nil uses the system roots; tests inject their own
}

func NewSMTPMailer(addr, user, pass, from string) (*smtpMailer, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("smtp address must be host:port: %w", err)
	}
	f, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("mail-from: %w", err)
	}
	_, port, _ := net.SplitHostPort(addr)
	return &smtpMailer{addr: addr, host: host, user: user, pass: pass, from: *f, timeout: 15 * time.Second, implicitTLS: port == "465"}, nil
}

func (*smtpMailer) Delivers() bool { return true }

func (s *smtpMailer) Send(ctx context.Context, m Mail) error {
	msg, err := s.build(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	if s.tlsConfig != nil {
		tlsCfg = s.tlsConfig
	}
	if s.implicitTLS {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if _, isTLS := conn.(*tls.Conn); !isTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp server does not offer STARTTLS, refusing to send credentials in clear text")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (s *smtpMailer) build(m Mail) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct{ ctype, content string }{
		{"text/plain; charset=UTF-8", m.Text},
		{"text/html; charset=UTF-8", m.HTML},
	} {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.ctype},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQuotedPrintable(w, part.content); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	var msg bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&msg, "%s: %s\r\n", k, v) }
	hdr("From", s.from.String())
	hdr("To", (&mail.Address{Address: m.To}).String())
	hdr("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<%s@%s>", hex.EncodeToString(id), s.host))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	msg.WriteString("\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes(), nil
}

func inviteMail(to, app, appURL, link string) Mail {
	text := fmt.Sprintf("%s was shared with you on nephos.\n\nOpen it: %s\n\nThis link works once and expires in 7 days. After that, go to %s and enter this email address to get a new one.\n\nIf you weren't expecting this, ignore this email.\n", app, link, appURL)
	return Mail{
		To:      to,
		Subject: fmt.Sprintf("%s was shared with you", app),
		Text:    text,
		HTML: mailHTML(
			fmt.Sprintf("%s was shared with you", app),
			"Open it with the button below. The link works once and expires in 7 days. After that, go to "+html.EscapeString(appURL)+" and enter this email address to get a new one.",
			"Open "+app, link,
			"If you weren't expecting this, ignore this email."),
	}
}

func loginMail(to, app, link string) Mail {
	text := fmt.Sprintf("Use this link to open %s: %s\n\nIt works once and expires in 15 minutes. If you didn't ask for it, ignore this email.\n", app, link)
	return Mail{
		To:      to,
		Subject: fmt.Sprintf("Your link to open %s", app),
		Text:    text,
		HTML: mailHTML(
			fmt.Sprintf("Open %s", app),
			"Use the button below. The link works once and expires in 15 minutes.",
			"Open "+app, link,
			"If you didn't ask for this, ignore this email."),
	}
}

func mailHTML(heading, lead, button, link, foot string) string {
	return fmt.Sprintf(`<!doctype html><html><body style="margin:0;padding:24px;background:#eef3f7;font:16px/1.55 system-ui,-apple-system,Segoe UI,sans-serif;color:#14222e">
<div style="max-width:30rem;margin:0 auto">
<p style="font:24px Georgia,serif;margin:0 0 20px">nephos</p>
<h1 style="font:26px/1.2 Georgia,serif;margin:0 0 12px">%s</h1>
<p style="margin:0 0 20px">%s</p>
<p style="margin:0 0 24px"><a href="%s" style="background:#edb02e;color:#14222e;text-decoration:none;font-weight:600;padding:10px 18px;border-radius:8px;display:inline-block">%s</a></p>
<p style="color:#566b7c;font-size:14px;margin:0">%s</p>
</div></body></html>`, html.EscapeString(heading), lead, html.EscapeString(link), html.EscapeString(button), html.EscapeString(foot))
}

func writeQuotedPrintable(w io.Writer, s string) error {
	qw := quotedprintable.NewWriter(w)
	if _, err := qw.Write([]byte(s)); err != nil {
		return err
	}
	return qw.Close()
}
