package protocol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
)

// ClassifyErr turns a transport error into a short, stable kind used to
// group errors in reports.
func ClassifyErr(err error) string {
	if err == nil {
		return ""
	}
	var se *StepError
	if errors.As(err, &se) {
		return se.Kind
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "conn_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "conn_reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "no_local_port"
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return "too_many_files"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "eof"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var certErr *tls.CertificateVerificationError
	var unk x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &unk) {
		return "tls_cert"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "Timeout"):
		return "timeout"
	case strings.Contains(msg, "tls"):
		return "tls"
	case strings.Contains(msg, "connection reset"):
		return "conn_reset"
	}
	return "error"
}
