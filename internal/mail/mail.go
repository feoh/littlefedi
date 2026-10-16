// Package mail provides email sending capabilities for littleFedi.
// Supports sending via SMTP when configured, or falling back to stdout logging.
package mail

import (
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net"
	stdmail "net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/i18n"
	"github.com/google/uuid"
)

// Mailer handles sending emails.
type Mailer struct {
	cfg         config.SMTP
	webDomain   string
	localDomain string
}

var errInvalidMessage = errors.New("invalid mail message")

func permanentSendError(err error) bool {
	if errors.Is(err, errInvalidMessage) {
		return true
	}
	var smtpErr *textproto.Error
	return errors.As(err, &smtpErr) && smtpErr.Code >= 500 && smtpErr.Code < 600
}

// New creates a new Mailer from the SMTP config.
// Returns nil if SMTP is not configured (no host set).
func New(cfg config.SMTP, webDomain, localDomain string) *Mailer {
	if cfg.Host == "" {
		return nil
	}
	if cfg.InsecureSkipVerify {
		slog.Warn("mail: SMTP certificate verification disabled")
	}
	return &Mailer{cfg: cfg, webDomain: webDomain, localDomain: localDomain}
}

// IsConfigured returns true if SMTP settings are present.
func (m *Mailer) IsConfigured() bool {
	return m != nil && m.cfg.Host != ""
}

// Send sends an email to the given recipient.
func (m *Mailer) Send(to, subject, body string) error {
	if !m.IsConfigured() {
		slog.Warn("mail: SMTP not configured, logging email to stdout",
			"to", to, "subject", subject)
		fmt.Printf("\n=== EMAIL to %s ===\nSubject: %s\n\n%s\n\n================\n", to, subject, body)
		return nil
	}

	from := m.cfg.From
	if from == "" {
		from = "littlefedi@" + extractDomain(m.cfg.Host)
	}
	toAddress, err := stdmail.ParseAddress(to)
	if err != nil || strings.ContainsAny(to, "\r\n") {
		return fmt.Errorf("%w: recipient address", errInvalidMessage)
	}
	fromAddress, err := stdmail.ParseAddress(from)
	if err != nil || strings.ContainsAny(from, "\r\n") {
		return fmt.Errorf("%w: sender address", errInvalidMessage)
	}
	if strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("%w: subject", errInvalidMessage)
	}

	var auth smtp.Auth
	if m.cfg.Username != "" || m.cfg.Password != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	}

	msg := buildMessage(fromAddress.String(), toAddress.String(), subject, body, m.localDomain)
	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)

	if err := m.sendMail(addr, auth, fromAddress.Address, toAddress.Address, msg); err != nil {
		return fmt.Errorf("send mail: %w", err)
	}

	slog.Info("mail: sent email", "to", to, "subject", subject)
	return nil
}

func (m *Mailer) sendMail(addr string, auth smtp.Auth, from, to, message string) error {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{
		ServerName:         m.cfg.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: m.cfg.InsecureSkipVerify, // #nosec G402 -- explicit opt-in for private/self-signed SMTP servers
	}

	var (
		conn net.Conn
		err  error
	)
	if m.cfg.ImplicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	client, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()

	if !m.cfg.ImplicitTLS {
		if supported, _ := client.Extension("STARTTLS"); supported {
			if err := client.StartTLS(tlsConfig); err != nil {
				return err
			}
		} else if m.cfg.RequireTLS {
			return fmt.Errorf("server does not support STARTTLS")
		}
	}
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("server does not support authentication")
		}
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, message); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// Convenience senders

// SendApprovalGranted sends an email notifying the user that their account was approved.
func (m *Mailer) SendApprovalGranted(locale, to, username string) error {
	subject := i18n.T(locale, "mail.approval_granted.subject", m.localDomain)
	body := i18n.T(locale, "mail.approval_granted.body", username, m.localDomain, m.webDomain, m.localDomain)
	return m.Send(to, subject, body)
}

// SendApprovalRejected sends an email notifying the user that their account was rejected.
func (m *Mailer) SendApprovalRejected(locale, to, username string) error {
	subject := i18n.T(locale, "mail.approval_rejected.subject", m.localDomain)
	body := i18n.T(locale, "mail.approval_rejected.body", username, m.localDomain, m.localDomain)
	return m.Send(to, subject, body)
}

// SendAdminNewReport sends an email to admins when a new moderation report is filed.
func (m *Mailer) SendAdminNewReport(to, reportedBy string) error {
	subject := fmt.Sprintf("[%s] New moderation report", m.localDomain)
	body := fmt.Sprintf(
		"A new moderation report has been filed on %s by @%s.\n\n"+
			"Review reports at:\n\n"+
			"https://%s/admin/reports\n\n"+
			"- littleFedi",
		m.localDomain, reportedBy, m.webDomain,
	)
	return m.Send(to, subject, body)
}

// Internal helpers

// buildMessage constructs a raw RFC 822 email message with a plain text and a
// styled HTML alternative so the mail stays readable everywhere.
func buildMessage(from, to, subject, body, domain string) string {
	boundary := "littlefedi-" + uuid.NewString()
	headers := make([]string, 0, 10)
	headers = append(headers, fmt.Sprintf("From: %s", from))
	headers = append(headers, fmt.Sprintf("To: %s", to))
	headers = append(headers, fmt.Sprintf("Subject: %s", mime.QEncoding.Encode("utf-8", subject)))
	headers = append(headers, "Date: "+time.Now().Format(time.RFC1123Z))
	headers = append(headers, fmt.Sprintf("Message-ID: <%s@%s>", uuid.NewString(), domain))
	headers = append(headers, "MIME-Version: 1.0")
	headers = append(headers, fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q", boundary))
	headers = append(headers, "")
	headers = append(headers, "--"+boundary)
	headers = append(headers, `Content-Type: text/plain; charset="utf-8"`)
	headers = append(headers, "Content-Transfer-Encoding: 8bit")
	headers = append(headers, "")
	headers = append(headers, body)
	headers = append(headers, "--"+boundary)
	headers = append(headers, `Content-Type: text/html; charset="utf-8"`)
	headers = append(headers, "Content-Transfer-Encoding: 8bit")
	headers = append(headers, "")
	headers = append(headers, htmlBody(body))
	headers = append(headers, "--"+boundary+"--")
	return strings.Join(headers, "\r\n")
}

var mailURLRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// htmlBody renders the plain text body as a styled HTML email matching the
// default web theme: a white rounded card on the theme background, teal accent
// links, and the littlefedi wordmark. No images are used.
func htmlBody(body string) string {
	var paragraphs strings.Builder
	for _, paragraph := range strings.Split(body, "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		paragraph = html.EscapeString(paragraph)
		paragraph = linkify(paragraph)
		paragraph = strings.ReplaceAll(paragraph, "\n", "<br>")
		paragraphs.WriteString(`<p style="margin:0 0 16px;">`)
		paragraphs.WriteString(paragraph)
		paragraphs.WriteString("</p>")
	}
	return strings.Replace(mailHTMLTemplate, "{{BODY}}", paragraphs.String(), 1)
}

// linkify turns bare http(s) URLs into accent-coloured links.
func linkify(s string) string {
	return mailURLRe.ReplaceAllStringFunc(s, func(u string) string {
		u = strings.TrimRight(u, ".,;:!?)")
		return `<a href="` + u + `" style="color:#0e9488;text-decoration:none;">` + u + `</a>`
	})
}

const mailHTMLTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
@media (prefers-color-scheme: dark) {
  .lf-body{background-color:#0c0f14 !important;}
  .lf-card{background-color:#151a22 !important;border-color:#272f3c !important;color:#e8edf5 !important;}
  .lf-card a{color:#2dd4bf !important;}
  .lf-wordmark{color:#e8edf5 !important;}
  .lf-wordmark b,.lf-footer b{color:#2dd4bf !important;}
  .lf-footer{color:#94a0b3 !important;}
}
</style>
</head>
<body class="lf-body" style="margin:0;padding:0;background-color:#f5f7fa;font-family:Inter,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Oxygen,Ubuntu,Cantarell,'Helvetica Neue',sans-serif;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="padding:32px 16px;">
<tr><td align="center">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="max-width:600px;width:100%;">
<tr><td style="padding-bottom:20px;">
<span style="display:inline-block;width:30px;height:30px;border-radius:9px;background-color:#0e9488;background-image:linear-gradient(135deg,#14b8a6 0%,#0e9488 55%,#0891b2 100%);color:#ffffff;text-align:center;line-height:30px;font-size:15px;">&#9998;</span>
<span class="lf-wordmark" style="display:inline-block;vertical-align:top;padding:5px 0 0 10px;font-size:18px;font-weight:800;letter-spacing:-0.02em;color:#171a21;">little<span style="color:#0e9488;">fedi</span></span>
</td></tr>
<tr><td>
<table class="lf-card" role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="background-color:#ffffff;border:1px solid #e6e9ef;border-radius:14px;color:#171a21;font-size:15px;line-height:1.6;">
<tr><td style="padding:24px 28px;">
{{BODY}}
</td></tr>
</table>
</td></tr>
<tr><td>
<p class="lf-footer" style="margin:20px 0 0;text-align:center;font-size:12px;color:#687082;">little<b style="color:#0e9488;">fedi</b></p>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`

// extractDomain extracts the domain part from a host string (without port).
func extractDomain(host string) string {
	if idx := strings.Index(host, ":"); idx >= 0 {
		return host[:idx]
	}
	return host
}
