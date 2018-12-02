// Input/output handling for GoAWK interpreter

package interp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"

	. "github.com/ktye/awk/internal/ast"
	. "github.com/ktye/awk/lexer"
)

// Print a line of output followed by a newline
func (p *interp) printLine(writer io.Writer, line string) error {
	err := writeOutput(writer, line)
	if err != nil {
		return err
	}
	return writeOutput(writer, p.outputRecordSep)
}

// Implement a buffered version of WriteCloser so output is buffered
// when redirecting to a file (eg: print >"out")
type bufferedWriteCloser struct {
	*bufio.Writer
	io.Closer
}

func newBufferedWriteClose(w io.WriteCloser) *bufferedWriteCloser {
	writer := bufio.NewWriterSize(w, outputBufSize)
	return &bufferedWriteCloser{writer, w}
}

func (wc *bufferedWriteCloser) Close() error {
	err := wc.Writer.Flush()
	if err != nil {
		return err
	}
	return wc.Closer.Close()
}

// Determine the output stream for given redirect token and
// destination (file or pipe name)
func (p *interp) getOutputStream(redirect Token, dest Expr) (io.Writer, error) {
	if redirect == ILLEGAL {
		// Token "ILLEGAL" means send to standard output
		return p.output, nil
	}

	destValue, err := p.eval(dest)
	if err != nil {
		return nil, err
	}
	name := p.toString(destValue)
	if _, ok := p.inputStreams[name]; ok {
		return nil, newError("can't write to reader stream")
	}
	if w, ok := p.outputStreams[name]; ok {
		return w, nil
	}

	switch redirect {
	case GREATER, APPEND:
		// Write or append to file
		flags := os.O_CREATE | os.O_WRONLY
		if redirect == GREATER {
			flags |= os.O_TRUNC
		} else {
			flags |= os.O_APPEND
		}
		w, err := os.OpenFile(name, flags, 0644)
		if err != nil {
			return nil, newError("output redirection error: %s", err)
		}
		buffered := newBufferedWriteClose(w)
		p.outputStreams[name] = buffered
		return buffered, nil

	case PIPE:
		// Pipe to command
		cmd := exec.Command("sh", "-c", name)
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, newError("error connecting to stdin pipe: %v", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, newError("error connecting to stdout pipe: %v", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, newError("error connecting to stderr pipe: %v", err)
		}
		err = cmd.Start()
		if err != nil {
			fmt.Fprintln(p.errorOutput, err)
			return ioutil.Discard, nil
		}
		go func() {
			io.Copy(p.output, stdout)
		}()
		go func() {
			io.Copy(p.errorOutput, stderr)
		}()
		p.commands[name] = cmd
		p.outputStreams[name] = w
		return w, nil

	default:
		// Should never happen
		panic(fmt.Sprintf("unexpected redirect type %s", redirect))
	}
}

// Get input Scanner to use for "getline" based on file name
func (p *interp) getInputScannerFile(name string) (*bufio.Scanner, error) {
	if _, ok := p.outputStreams[name]; ok {
		return nil, newError("can't read from writer stream")
	}
	if _, ok := p.inputStreams[name]; ok {
		return p.scanners[name], nil
	}
	r, err := os.Open(name)
	if err != nil {
		return nil, newError("input redirection error: %s", err)
	}
	scanner := p.newScanner(r)
	p.scanners[name] = scanner
	p.inputStreams[name] = r
	return scanner, nil
}

// Get input Scanner to use for "getline" based on pipe name
func (p *interp) getInputScannerPipe(name string) (*bufio.Scanner, error) {
	if _, ok := p.outputStreams[name]; ok {
		return nil, newError("can't read from writer stream")
	}
	if _, ok := p.inputStreams[name]; ok {
		return p.scanners[name], nil
	}
	cmd := exec.Command("sh", "-c", name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, newError("error connecting to stdin pipe: %v", err)
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, newError("error connecting to stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, newError("error connecting to stderr pipe: %v", err)
	}
	err = cmd.Start()
	if err != nil {
		fmt.Fprintln(p.errorOutput, err)
		return bufio.NewScanner(strings.NewReader("")), nil
	}
	go func() {
		io.Copy(stdin, p.stdin)
		stdin.Close()
	}()
	go func() {
		io.Copy(p.errorOutput, stderr)
	}()
	scanner := p.newScanner(r)
	p.commands[name] = cmd
	p.inputStreams[name] = r
	p.scanners[name] = scanner
	return scanner, nil
}

// Create a new buffered Scanner for reading input records
func (p *interp) newScanner(input io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(input)
	switch p.recordSep {
	case "\n":
		// Scanner default is to split on newlines
	case "":
		// Empty string for RS means split on \n\n (blank lines)
		scanner.Split(scanLinesBlank)
	default:
		splitter := byteSplitter{p.recordSep[0]}
		scanner.Split(splitter.scan)
	}
	buffer := make([]byte, inputBufSize)
	scanner.Buffer(buffer, maxRecordLength)
	return scanner
}

// Copied from bufio/scan.go in the stdlib: I guess it's a bit more
// efficient than bytes.TrimSuffix(data, []byte("\r"))
func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[:len(data)-1]
	}
	return data
}

func dropLF(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		return data[:len(data)-1]
	}
	return data
}

func scanLinesBlank(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// Skip newlines at beginning of data
	i := 0
	for i < len(data) && (data[i] == '\n' || data[i] == '\r') {
		i++
	}
	start := i

	// Try to find two consecutive newlines (or \n\r\n for Windows)
	for ; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		end := i
		if i+1 < len(data) && data[i+1] == '\n' {
			i += 2
			for i < len(data) && (data[i] == '\n' || data[i] == '\r') {
				i++ // Skip newlines at end of record
			}
			return i, dropCR(data[start:end]), nil
		}
		if i+2 < len(data) && data[i+1] == '\r' && data[i+2] == '\n' {
			i += 3
			for i < len(data) && (data[i] == '\n' || data[i] == '\r') {
				i++ // Skip newlines at end of record
			}
			return i, dropCR(data[start:end]), nil
		}
	}

	// If we're at EOF, we have one final record; return it
	if atEOF {
		return len(data), dropCR(dropLF(data)), nil
	}

	// Request more data
	return 0, nil, nil
}

// Splitter function that splits records on the given separator byte
type byteSplitter struct {
	sep byte
}

func (s byteSplitter) scan(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, s.sep); i >= 0 {
		// We have a full sep-terminated record
		return i + 1, data[0:i], nil
	}
	// If at EOF, we have a final, non-terminated record; return it
	if atEOF {
		return len(data), data, nil
	}
	// Request more data
	return 0, nil, nil
}

// Setup for a new input file with given name (empty string if stdin)
func (p *interp) setFile(filename string) {
	p.filename = filename
	p.fileLineNum = 0
}

// Setup for a new input line, and parse it into fields
func (p *interp) setLine(line string) {
	p.line = line
	if p.fieldSep == " " {
		// FS space (default) means split fields on any whitespace
		p.fields = strings.Fields(line)
	} else if line == "" {
		p.fields = nil
	} else {
		// Split on FS as a regex
		p.fields = p.fieldSepRegex.Split(line, -1)

		// Special case for when RS=="" and FS is single character,
		// split on newline in addition to FS. See more here:
		// https://www.gnu.org/software/gawk/manual/html_node/Multiple-Line.html
		if p.recordSep == "" && len(p.fieldSep) == 1 {
			fields := make([]string, 0, len(p.fields))
			for _, field := range p.fields {
				lines := strings.Split(field, "\n")
				for _, line := range lines {
					trimmed := strings.TrimSuffix(line, "\r")
					fields = append(fields, trimmed)
				}
			}
			p.fields = fields
		}
	}
	p.numFields = len(p.fields)
}

// Fetch next line (record) of input from current input file, opening
// next input file if done with previous one
func (p *interp) nextLine() (string, error) {
	for {
		if p.scanner == nil {
			if prevInput, ok := p.input.(io.Closer); ok && p.input != p.stdin {
				// Previous input is file, close it
				prevInput.Close()
			}
			if p.filenameIndex >= p.argc && !p.hadFiles {
				// Moved past number of ARGV args and haven't seen
				// any files yet, use stdin
				p.input = p.stdin
				p.setFile("")
				p.hadFiles = true
			} else {
				if p.filenameIndex >= p.argc {
					// Done with ARGV args, all done with input
					return "", io.EOF
				}
				// Fetch next filename from ARGV
				index := strconv.Itoa(p.filenameIndex)
				argvIndex := p.program.Arrays["ARGV"]
				filename := p.toString(p.getArrayValue(ScopeGlobal, argvIndex, index))
				p.filenameIndex++

				// Is it actually a var=value assignment?
				matches := varRegex.FindStringSubmatch(filename)
				if len(matches) >= 3 {
					// Yep, set variable to value and keep going
					err := p.setVarByName(matches[1], matches[2])
					if err != nil {
						return "", err
					}
					continue
				} else if filename == "" {
					// ARGV arg is empty string, skip
					p.input = nil
					continue
				} else if filename == "-" {
					// ARGV arg is "-" meaning stdin
					p.input = p.stdin
					p.setFile("")
				} else {
					// A regular file name, open it
					input, err := os.Open(filename)
					if err != nil {
						return "", err
					}
					p.input = input
					p.setFile(filename)
					p.hadFiles = true
				}
			}
			p.scanner = p.newScanner(p.input)
		}
		if p.scanner.Scan() {
			// We scanned some input, break and return it
			break
		}
		if err := p.scanner.Err(); err != nil {
			return "", fmt.Errorf("error reading from input: %s", err)
		}
		// Signal loop to move onto next file
		p.scanner = nil
	}

	// Got a line (record) of input, return it
	p.lineNum++
	p.fileLineNum++
	return p.scanner.Text(), nil
}

// Write output string to given writer, producing correct line endings
// on Windows (CR LF)
func writeOutput(w io.Writer, s string) error {
	if crlfNewline {
		// First normalize to \n, then convert all newlines to \r\n
		// (on Windows). NOTE: creating two new strings is almost
		// certainly slow; would be better to create a custom Writer.
		s = strings.Replace(s, "\r\n", "\n", -1)
		s = strings.Replace(s, "\n", "\r\n", -1)
	}
	_, err := io.WriteString(w, s)
	return err
}

// Close all streams, commands, etc (after program execution)
func (p *interp) closeAll() {
	if prevInput, ok := p.input.(io.Closer); ok {
		prevInput.Close()
	}
	for _, r := range p.inputStreams {
		_ = r.Close()
	}
	for _, w := range p.outputStreams {
		_ = w.Close()
	}
	for _, cmd := range p.commands {
		_ = cmd.Wait()
	}
	if p.flushOutput {
		p.output.(*bufio.Writer).Flush()
	}
	if p.flushError {
		p.errorOutput.(*bufio.Writer).Flush()
	}
}
