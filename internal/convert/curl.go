package convert

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// curl options that take a value we don't use.
var curlSkipArg = map[string]bool{
	"-o": true, "--output": true, "-m": true, "--max-time": true, "--connect-timeout": true,
	"-w": true, "--write-out": true, "--retry": true, "--retry-delay": true, "--retry-max-time": true,
	"-x": true, "--proxy": true, "-U": true, "--proxy-user": true, "--resolve": true,
	"--connect-to": true, "--cacert": true, "--capath": true, "-E": true, "--cert": true,
	"--key": true, "--cert-type": true, "--key-type": true, "-c": true, "--cookie-jar": true,
	"-r": true, "--range": true, "--limit-rate": true, "-A": true, "--user-agent": true,
	"-e": true, "--referer": true, "-K": true, "--config": true, "--interface": true,
	"--max-redirs": true, "--keepalive-time": true, "--dns-servers": true, "-y": true,
	"--speed-time": true, "-Y": true, "--speed-limit": true, "--trace": true,
	"--trace-ascii": true, "--stderr": true, "-D": true, "--dump-header": true,
	"--ciphers": true, "--tls-max": true, "--expect100-timeout": true, "--unix-socket": true,
	"--aws-sigv4": true, "--oauth2-bearer": true, "-z": true, "--time-cond": true,
}

// curl short options that take a value.
const curlShortArg = "XHdbuFTomwxUrAecKEDyYz"

// Curl converts one or more curl commands (e.g. DevTools "Copy as cURL").
func Curl(data []byte, opt Options) (*Scenario, error) {
	cmds, err := splitCommands(string(data))
	if err != nil {
		return nil, fmt.Errorf("curl: %w", err)
	}
	if len(cmds) == 0 {
		return nil, fmt.Errorf("curl: no curl commands found")
	}
	s := newScenario(opt)
	var reqs []request
	skipped := 0
	for i, args := range cmds {
		r, insecure, err := parseCurl(args)
		if err != nil {
			return nil, fmt.Errorf("curl command %d: %w", i+1, err)
		}
		if !hostAllowed(r.URL, opt.Hosts) {
			skipped++
			continue
		}
		s.Insecure = s.Insecure || insecure
		reqs = append(reqs, r)
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("curl: no commands left after --host filter")
	}
	s.Comments = append(s.Comments, fmt.Sprintf("Converted from curl by lt convert: %d request(s).", len(reqs)))
	if skipped > 0 {
		s.Comments = append(s.Comments, fmt.Sprintf("skipped %d request(s) to other hosts (--host)", skipped))
	}
	s.Comments = append(s.Comments, "Review hard-coded tokens and ids; replace them with vars, data or extract.")
	buildSteps(s, reqs, true, opt.BaseURL)
	return s, nil
}

func parseCurl(args []string) (request, bool, error) {
	var (
		r        request
		method   string
		rawURL   string
		data     []string
		get      bool
		head     bool
		insecure bool
		warn     []string
	)
	next := func(i *int, name string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := a, "", false
		switch {
		case strings.HasPrefix(a, "--"):
			if k, v, ok := strings.Cut(a, "="); ok {
				name, val, hasVal = k, v, true
			}
		case strings.HasPrefix(a, "-") && len(a) > 2:
			// -XPOST, or bundled flags like -sSLk
			if strings.IndexByte(curlShortArg, a[1]) >= 0 {
				name, val, hasVal = a[:2], a[2:], true
			} else {
				var rest []string
				for j := 1; j < len(a); j++ {
					f := "-" + string(a[j])
					if strings.IndexByte(curlShortArg, a[j]) >= 0 {
						rest = append(rest, f)
						if v := a[j+1:]; v != "" {
							rest = append(rest, v)
						}
						break
					}
					rest = append(rest, f)
				}
				args = append(args[:i], append(rest, args[i+1:]...)...)
				i--
				continue
			}
		case !strings.HasPrefix(a, "-") || a == "-":
			if rawURL == "" {
				rawURL = a
			}
			continue
		}
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			return next(&i, name)
		}
		var err error
		switch name {
		case "-X", "--request":
			method, err = value()
		case "--url":
			rawURL, err = value()
		case "-H", "--header":
			var h string
			if h, err = value(); err == nil {
				k, v, ok := strings.Cut(h, ":")
				if ok {
					r.Headers = append(r.Headers, KV{strings.TrimSpace(k), strings.TrimSpace(v)})
				}
			}
		case "-d", "--data", "--data-ascii", "--data-binary":
			var d string
			if d, err = value(); err == nil {
				if strings.HasPrefix(d, "@") {
					d, err = readData(d[1:], name == "--data-binary")
				}
				data = append(data, d)
			}
		case "--data-raw":
			var d string
			if d, err = value(); err == nil {
				data = append(data, d)
			}
		case "--data-urlencode":
			var d string
			if d, err = value(); err == nil {
				d, err = urlencodeData(d)
				data = append(data, d)
			}
		case "--json":
			var d string
			if d, err = value(); err == nil {
				if strings.HasPrefix(d, "@") {
					d, err = readData(d[1:], true)
				}
				data = append(data, d)
				if headerValue(r.Headers, "Content-Type") == "" {
					r.Headers = append(r.Headers, KV{"Content-Type", "application/json"})
				}
				if headerValue(r.Headers, "Accept") == "" {
					r.Headers = append(r.Headers, KV{"Accept", "application/json"})
				}
			}
		case "-u", "--user":
			var up string
			if up, err = value(); err == nil {
				r.Headers = append(r.Headers, KV{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(up))})
			}
		case "-b", "--cookie":
			var c string
			if c, err = value(); err == nil {
				if strings.Contains(c, "=") {
					r.Headers = append(r.Headers, KV{"Cookie", c})
				} else {
					warn = append(warn, "cookie file "+c+" ignored")
				}
			}
		case "-F", "--form", "--form-string", "-T", "--upload-file":
			_, err = value()
			warn = append(warn, name+" (multipart/upload) is not converted")
		case "-G", "--get":
			get = true
		case "-I", "--head":
			head = true
		case "-k", "--insecure":
			insecure = true
		default:
			if curlSkipArg[name] && !hasVal {
				_, err = next(&i, name)
			}
			// Other flags (-s, -L, --compressed, -v, ...) don't affect the request.
		}
		if err != nil {
			return r, false, err
		}
	}
	if rawURL == "" {
		return r, false, fmt.Errorf("no URL")
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return r, false, err
	}
	body := strings.Join(data, "&")
	if get && body != "" {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += body
		body = ""
	}
	r.URL = u
	switch {
	case method != "":
		r.Method = strings.ToUpper(method)
	case head:
		r.Method = "HEAD"
	case body != "":
		r.Method = "POST"
	default:
		r.Method = "GET"
	}
	if body != "" {
		r.Body = body
		if headerValue(r.Headers, "Content-Type") == "" {
			r.contentType = "application/x-www-form-urlencoded" // what curl sends
			r.Headers = append(r.Headers, KV{"Content-Type", r.contentType})
		}
	}
	if len(warn) > 0 {
		r.Comment = "note: " + strings.Join(warn, "; ")
	}
	return r, insecure, nil
}

func readData(file string, binary bool) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	s := string(b)
	if !binary { // -d strips newlines from files
		s = strings.NewReplacer("\r", "", "\n", "").Replace(s)
	}
	return s, nil
}

// urlencodeData implements --data-urlencode's content forms.
func urlencodeData(d string) (string, error) {
	if strings.Contains(d, "@") && !strings.Contains(d, "=") {
		return "", fmt.Errorf("--data-urlencode with a file is not supported")
	}
	if k, v, ok := strings.Cut(d, "="); ok {
		if k == "" {
			return url.QueryEscape(v), nil
		}
		return k + "=" + url.QueryEscape(v), nil
	}
	return url.QueryEscape(d), nil
}

// splitCommands tokenizes shell text into curl commands. Commands end at an
// unquoted newline, ";", "&&" or "|"; "\" + newline continues a line.
func splitCommands(src string) ([][]string, error) {
	var (
		cmds [][]string
		cur  []string
		tok  strings.Builder
		in   bool // inside a token
	)
	endTok := func() {
		if in {
			cur = append(cur, tok.String())
			tok.Reset()
			in = false
		}
	}
	endCmd := func() {
		endTok()
		if len(cur) > 0 && cur[0] == "curl" {
			cmds = append(cmds, cur)
		}
		cur = nil
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src) && (src[i+1] == '\n' || src[i+1] == '\r'):
			i++
			if src[i] == '\r' && i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
		case c == '\\' && i+1 < len(src):
			i++
			tok.WriteByte(src[i])
			in = true
		case c == '#' && !in:
			for i < len(src) && src[i] != '\n' {
				i++
			}
			endCmd()
		case c == '\n' || c == ';' || c == '|':
			endCmd()
		case c == '&' && i+1 < len(src) && src[i+1] == '&':
			i++
			endCmd()
		case c == ' ' || c == '\t' || c == '\r':
			endTok()
		case c == '\'':
			j := strings.IndexByte(src[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			tok.WriteString(src[i+1 : i+1+j])
			in = true
			i += j + 1
		case c == '$' && i+1 < len(src) && src[i+1] == '\'':
			s, n, err := ansiC(src[i+2:])
			if err != nil {
				return nil, err
			}
			tok.WriteString(s)
			in = true
			i += n + 1
		case c == '"':
			i++
			for ; i < len(src) && src[i] != '"'; i++ {
				if src[i] == '\\' && i+1 < len(src) && strings.IndexByte("\"\\$`\n", src[i+1]) >= 0 {
					i++
					if src[i] == '\n' {
						continue
					}
				}
				tok.WriteByte(src[i])
			}
			if i >= len(src) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			in = true
		default:
			tok.WriteByte(c)
			in = true
		}
	}
	endCmd()
	return cmds, nil
}

// ansiC decodes the body of $'...' and returns it and the bytes consumed
// including the closing quote.
func ansiC(s string) (string, int, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			return b.String(), i + 1, nil
		}
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case 'x', 'u', 'U':
			max := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			j := i + 1
			for j < len(s) && j-i-1 < max && isHex(s[j]) {
				j++
			}
			if j == i+1 {
				b.WriteByte('\\')
				b.WriteByte(e)
				continue
			}
			v, _ := strconv.ParseUint(s[i+1:j], 16, 32)
			if e == 'x' {
				b.WriteByte(byte(v))
			} else {
				b.WriteRune(rune(v))
			}
			i = j - 1
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(s) && j-i < 3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[i:j], 8, 8)
			b.WriteByte(byte(v))
			i = j - 1
		default: // \\ \' \" \? and unknown escapes
			if strings.IndexByte("\\'\"?", e) < 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(e)
		}
	}
	return "", 0, fmt.Errorf("unterminated $'...' quote")
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
