package console

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/moby/term"
)

// Relay runs bidirectional I/O with escape-sequence detection.
// rw can be a PTY (*os.File) or a Unix socket (net.Conn) — any io.ReadWriter.
// escapeKeys is the byte sequence that triggers a detach (e.g. {0x1D, '.'}).
// Returns nil on clean disconnect (escape sequence, EOF, or EIO).
//
// The caller is responsible for closing the underlying connection after Relay
// returns, which unblocks the remaining goroutine.
func Relay(rw io.ReadWriter, escapeKeys []byte) error {
	errCh := make(chan error, 2) //nolint:mnd

	go func() {
		_, err := io.Copy(os.Stdout, rw)
		errCh <- err
	}()

	go func() {
		var r io.Reader = os.Stdin
		if len(escapeKeys) > 0 {
			r = term.NewEscapeProxy(os.Stdin, escapeKeys)
		}
		_, err := io.Copy(rw, r)
		errCh <- err
	}()

	// Wait for the first goroutine to finish. The caller's defer conn.Close()
	// unblocks the other goroutine after Relay returns.
	err := <-errCh
	if isCleanExit(err) {
		return nil
	}
	return err
}

// FormatEscapeChar returns a human-readable representation of the escape byte.
func FormatEscapeChar(b byte) string {
	if b >= 1 && b <= 0x1F {
		return "^" + string(rune(b+'@'))
	}
	return string(b)
}

// ParseEscapeChar parses the --escape-char flag value. It accepts:
//   - Caret notation for control characters: "^]", "^A", "^C", etc.
//   - A single printable or control character (raw byte).
func ParseEscapeChar(s string) (byte, error) {
	if len(s) == 2 && s[0] == '^' {
		c := s[1]
		if c >= '@' && c <= '_' {
			return validateEscapeByte(c - '@')
		}
		if c >= 'a' && c <= 'z' {
			return validateEscapeByte(c - 'a' + 1)
		}
		return 0, fmt.Errorf("invalid caret notation %q (use ^A through ^_ or ^a through ^z)", s)
	}
	if len(s) == 1 {
		return validateEscapeByte(s[0])
	}
	return 0, fmt.Errorf("escape-char must be a single character or ^X caret notation, got %q", s)
}

func validateEscapeByte(b byte) (byte, error) {
	switch {
	case b == 0:
		return 0, fmt.Errorf("nul cannot be used as escape character")
	case b == '\r' || b == '\n':
		return 0, fmt.Errorf("cr/lf cannot be used as escape character")
	case b == 0x7F: //nolint:mnd
		return 0, fmt.Errorf("del (0x7f) cannot be used as escape character")
	case b >= 0x80: //nolint:mnd
		return 0, fmt.Errorf("non-ASCII byte 0x%02X cannot be used as escape character", b)
	}
	return b, nil
}

// isCleanExit returns true for errors that indicate a normal disconnect.
func isCleanExit(err error) bool {
	if err == nil {
		return true
	}
	var escErr term.EscapeError
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.As(err, &escErr)
}
