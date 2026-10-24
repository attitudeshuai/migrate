package migrate

import (
	"bufio"
	"bytes"
	"io"

	"github.com/golang-migrate/migrate/v4/database/multistmt"
)

// multiStatementDelimiter separates statements in a multi-statement
// migration, matching the delimiter used by the database drivers'
// multi-statement parsers.
const multiStatementDelimiter = ";"

// maxMultiStatementSize bounds a single statement the reader frames; it only
// needs to be at least as large as the limits configured on the drivers.
const maxMultiStatementSize = 100 * 1024 * 1024 // 100 MB

// statementReader wraps a multi-statement migration body and frames its
// Read results on statement delimiter boundaries. Each Read therefore hands
// the database driver at most one complete statement.
//
// Drivers parse and execute statements sequentially (see database/multistmt):
// the next statement is only read after the previous one executed. Framing
// reads this way keeps delivered aligned with the statement the driver is
// executing, so when Run fails the ordinal position of the failing statement
// within the migration text is known on top of the driver's own line number.
type statementReader struct {
	scanner *bufio.Scanner

	// pending holds the bytes of the statement currently being served.
	pending []byte

	// delivered is the number of complete statements handed to the driver.
	delivered int
}

func newStatementReader(r io.Reader) *statementReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, multistmt.StartBufSize), maxMultiStatementSize)
	s.Split(splitAtDelimiter([]byte(multiStatementDelimiter)))
	return &statementReader{scanner: s}
}

// Read serves at most one statement from the underlying body. Statements
// longer than p are served across several Read calls.
func (r *statementReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if len(r.pending) == 0 {
		if !r.scanner.Scan() {
			// scanner.Err returns nil at end of input; io.Reader callers
			// must see io.EOF rather than (0, nil).
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		r.pending = r.scanner.Bytes()
	}

	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	if len(r.pending) == 0 {
		r.delivered++
	}
	return n, nil
}

// statementsDelivered returns the 1-based index of the last statement handed
// to the driver, or 0 if no statement was delivered yet.
func (r *statementReader) statementsDelivered() int {
	return r.delivered
}

// splitAtDelimiter mirrors multistmt.splitWithDelimiter, which is not
// exported: the returned token keeps its trailing delimiter.
func splitAtDelimiter(delimiter []byte) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (int, []byte, error) {
		// SplitFunc inspired by bufio.ScanLines() implementation
		if atEOF {
			if len(data) == 0 {
				return 0, nil, nil
			}
			return len(data), data, nil
		}
		if i := bytes.Index(data, delimiter); i >= 0 {
			return i + len(delimiter), data[:i+len(delimiter)], nil
		}
		return 0, nil, nil
	}
}
