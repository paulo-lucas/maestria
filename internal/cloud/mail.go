package cloud

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Mailer sends email. Implementations must not block the caller for long: the server sends from a
// background queue so response time never reveals whether an address exists.
type Mailer interface {
	Send(m Message) error
}

// DevMailer writes emails instead of sending them (stdout or a file), for local development.
type DevMailer struct {
	mu  sync.Mutex
	out io.Writer
}

// NewDevMailer writes to w.
func NewDevMailer(w io.Writer) *DevMailer { return &DevMailer{out: w} }

// Send writes the message.
func (d *DevMailer) Send(m Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := fmt.Fprintf(d.out, "---- email ----\nTo: %s\nSubject: %s\n\n%s\n---- end ----\n", m.To, m.Subject, m.Body)
	return err
}

// SMTPMailer sends through net/smtp.
type SMTPMailer struct{ Cfg SMTP }

// Send delivers one message. Plain-text only, with STARTTLS (or implicit TLS) required whenever
// credentials are set.
func (s *SMTPMailer) Send(m Message) error {
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("invalid header value")
	}
	envelope, fromHeader, err := parseSender(s.Cfg.From)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.Cfg.Host, fmt.Sprint(s.Cfg.Port))
	msg := "From: " + fromHeader + "\r\nTo: " + m.To + "\r\nSubject: " + m.Subject +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nDate: " + time.Now().Format(time.RFC1123Z) +
		"\r\n\r\n" + strings.ReplaceAll(m.Body, "\n", "\r\n") + "\r\n"
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if s.Cfg.TLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.Cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, s.Cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if !s.Cfg.TLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.Cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return err
			}
		} else if s.Cfg.Username != "" {
			return errors.New("the SMTP server does not offer STARTTLS; refusing to send credentials in clear text")
		}
	}
	if s.Cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Cfg.Username, s.Cfg.Password, s.Cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(envelope); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// parseSender accepts "no-reply@example.com" or "Maestria <no-reply@example.com>". It returns the
// bare address for the SMTP envelope (MAIL FROM takes only the address) and the header form.
func parseSender(from string) (envelope, header string, err error) {
	a, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil {
		return "", "", fmt.Errorf("invalid sender address %q (use name@example.com or \"Name <name@example.com>\"; in a systemd environment file put the whole value in double quotes): %w", from, err)
	}
	return a.Address, a.String(), nil
}

// NewMailer builds the mailer from config: dev mode wins, then SMTP.
func NewMailer(c *Config, stdout io.Writer) (Mailer, error) {
	switch {
	case c.DevEmail == "stdout":
		return NewDevMailer(stdout), nil
	case strings.HasPrefix(c.DevEmail, "file:"):
		f, err := os.OpenFile(strings.TrimPrefix(c.DevEmail, "file:"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return NewDevMailer(f), nil
	case c.SMTP.Host != "":
		if c.SMTP.From == "" {
			return nil, errors.New("smtp.from is required")
		}
		return &SMTPMailer{Cfg: c.SMTP}, nil
	}
	return nil, errors.New("no email configured: set smtp.* or devEmail (\"stdout\")")
}

// mailQueue sends messages from one background goroutine.
type mailQueue struct {
	m    Mailer
	ch   chan Message
	log  *slog.Logger
	done chan struct{}
}

func newMailQueue(m Mailer, log *slog.Logger) *mailQueue {
	q := &mailQueue{m: m, ch: make(chan Message, 64), log: log, done: make(chan struct{})}
	go func() {
		defer close(q.done)
		for msg := range q.ch {
			if err := m.Send(msg); err != nil {
				log.Warn("email not sent", "error", err.Error()) // never the address or the body
			}
		}
	}()
	return q
}

// Enqueue never blocks: when the queue is full the message is dropped and logged.
func (q *mailQueue) Enqueue(m Message) {
	select {
	case q.ch <- m:
	default:
		q.log.Warn("email queue full; message dropped")
	}
}

func (q *mailQueue) Close() {
	close(q.ch)
	<-q.done
}
