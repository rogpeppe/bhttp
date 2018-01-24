// Go clone of http(1)
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	flag "github.com/juju/gnuflag"
	"github.com/juju/loggo"
	"github.com/juju/persistent-cookiejar"
	"github.com/rogpeppe/rjson"
	errgo "gopkg.in/errgo.v1"
	"gopkg.in/macaroon-bakery.v2/httpbakery"
	"gopkg.in/macaroon-bakery.v2/httpbakery/agent"
)

var logger = loggo.GetLogger("bhttp")

const helpMessage = `usage: http [flag...] [METHOD] URL [REQUEST_ITEM [REQUEST_ITEM...]]

  METHOD
      The HTTP method to be used for the request (GET, POST, PUT, DELETE, ...).
      
      This argument can be omitted in which case HTTPie will use POST if there
      is some data to be sent, otherwise GET:
      
          $ http example.org               # => GET
          $ http example.org hello=world   # => POST

  URL
      The scheme defaults to 'http://' if the URL does not include one.
      
      You can also use a shorthand for localhost
      
          $ http :3000                    # => http://localhost:3000
          $ http :/foo                    # => http://localhost/foo

  REQUEST_ITEM
      Optional key-value pairs to be included in the request. The separator used
      determines the type:
      
      ':' HTTP headers:
      
          Referer:http://httpie.org  Cookie:foo=bar  User-Agent:bacon/1.0
      
      '==' URL parameters to be appended to the request URI:
      
          search==httpie
      
      '=' Data fields to be serialized into a JSON object (with --json, -j)
          or form data (with --form, -f):
      
          name=HTTPie  language=Python  description='CLI HTTP client'
      
      ':=' Non-string JSON data fields (only with --json, -j):
      
          awesome:=true  amount:=42  colors:='["red", "green", "blue"]'
      
      '@' Form file fields (only with --form, -f): (NOT YET SUPPORTED)

          cs@~/Documents/CV.pdf
      
      '=@' A data field like '=', but takes a file path and embeds its content:
      
           essay=@Documents/essay.txt
      
      ':=@' A raw JSON field like ':=', but takes a file path and embeds its content:
      
          package:=@./package.json
      
      You can use a backslash to escape a colliding separator in the field name:
      
          field-name-with\:colon=value
`

type params struct {
	json        bool
	form        bool
	headers     bool
	body        bool
	rjson       bool
	raw         bool
	debug       bool
	noBrowser   bool
	basicAuth   string
	cookieFile  string
	agentFile   string
	useStdin    bool
	insecure    bool
	checkStatus bool
	// TODO auth, verify, proxy, file, timeout

	url     *url.URL
	method  string
	keyVals []keyVal
}

type request struct {
	url       *url.URL
	stdin     io.Reader
	method    string
	header    http.Header
	urlValues url.Values
	form      url.Values
	jsonObj   map[string]interface{}
	body      io.ReadSeeker
}

var errUsage = errors.New("bad usage")

type exitError struct {
	code int
}

func (e *exitError) Error() string {
	return fmt.Sprintf("exit with code %d", e.code)
}

type keyVal struct {
	key string
	sep string
	val string
}

func main() {
	err := main0()
	if err == nil {
		return
	}
	if err, ok := err.(*exitError); ok {
		os.Exit(err.code)
	}
	fmt.Fprintf(os.Stderr, "%v\n", err)
	os.Exit(1)
}

func main0() error {
	fset := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	req, p, err := newRequest(fset, os.Args[1:])
	if err != nil {
		if err == errUsage {
			fset.Usage()
		} else {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		return &exitError{2}
	}
	jar, client, err := newClient(p)
	if err != nil {
		fatalf("cannot make HTTP client: %v", err)
	}
	if jar != nil {
		defer jar.Save()
	}
	var stdin io.Reader
	if p.useStdin {
		stdin = os.Stdin
	}
	resp, err := req.do(client, stdin)
	if err != nil {
		return errgo.Mask(err)
	}
	defer resp.Body.Close()
	if err := showResponse(p, resp, os.Stdout); err != nil {
		return errgo.Mask(err)
	}
	statusClass := resp.StatusCode / 100
	if p.checkStatus && statusClass != 2 {
		return &exitError{statusClass}
	}
	return nil
}

func newRequest(fset *flag.FlagSet, args []string) (*request, *params, error) {
	p, err := parseArgs(fset, args)
	if err != nil {
		return nil, nil, err
	}
	if p.debug {
		loggo.ConfigureLoggers("DEBUG")
		http.DefaultTransport = loggingTransport{
			transport: http.DefaultTransport,
			printf: func(f string, a ...interface{}) {
				fmt.Fprintf(os.Stderr, f, a...)
			},
		}
	}
	req := &request{
		url:       p.url,
		method:    p.method,
		header:    make(http.Header),
		urlValues: make(url.Values),
		form:      make(url.Values),
		jsonObj:   make(map[string]interface{}),
	}
	for _, kv := range p.keyVals {
		if err := req.addKeyVal(p, kv); err != nil {
			return nil, nil, err
		}
	}
	if p.useStdin && (len(req.form) > 0 || len(req.jsonObj) > 0) {
		return nil, nil, errors.New("cannot read body from stdin when form or JSON body is specified")
	}
	if p.basicAuth != "" {
		req.header.Set("Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(p.basicAuth)))
	}
	if p.json && req.header.Get("Content-Type") == "" {
		req.header.Set("Content-Type", "application/json")
	}
	return req, p, nil
}

func parseArgs(fset *flag.FlagSet, args []string) (*params, error) {
	var p params
	var printHeaders, noBody, noCookies bool
	fset.BoolVar(&p.json, "j", false, "serialize  data  items  as a JSON object")
	fset.BoolVar(&p.json, "json", false, "")

	fset.BoolVar(&p.form, "f", false, "serialize data items as form values")
	fset.BoolVar(&p.form, "form", false, "")

	fset.BoolVar(&printHeaders, "h", false, "print the response headers")
	fset.BoolVar(&printHeaders, "headers", false, "")

	fset.BoolVar(&noBody, "B", false, "do not print response body")
	fset.BoolVar(&noBody, "body", false, "")

	fset.BoolVar(&p.debug, "debug", false, "print debugging messages, including all HTTP messages")

	fset.BoolVar(&p.noBrowser, "W", false, "do not open macaroon-login URLs in web browser")
	fset.BoolVar(&p.noBrowser, "no-browser", false, "")

	fset.BoolVar(&p.raw, "raw", false, "print response body without any JSON post-processing")

	fset.StringVar(&p.agentFile, "agent", "", "file to get agent keys from (implies agent authentication when possible)")

	fset.StringVar(&p.basicAuth, "a", "", "http basic auth (username:password)")
	fset.StringVar(&p.basicAuth, "auth", "", "")

	fset.BoolVar(&p.insecure, "insecure", false, "skip HTTPS certificate checking")

	fset.BoolVar(&p.checkStatus, "check-status", false, "if the HTTP status is not 2xx, print a warning and use the first digit of the status code as the exit code")

	fset.StringVar(&p.cookieFile, "cookiefile", cookiejar.DefaultCookieFile(), "file to store persistent cookies in")

	fset.BoolVar(&noCookies, "C", false, "disable cookie storage")
	fset.BoolVar(&noCookies, "no-cookies", false, "")

	fset.BoolVar(&p.useStdin, "stdin", false, "read request body from standard input")

	// TODO --file (multipart upload)
	// TODO --timeout
	// TODO --proxy
	// TODO (??) --verify

	fset.Usage = func() {
		fmt.Fprint(os.Stderr, helpMessage)
		fset.PrintDefaults()
	}
	if err := fset.Parse(true, args); err != nil {
		return nil, err
	}
	if noCookies {
		p.cookieFile = ""
	}
	p.headers = printHeaders
	p.body = !noBody
	args = fset.Args()
	if len(args) == 0 {
		return nil, errUsage
	}
	if isMethod(args[0]) {
		p.method, args = strings.ToUpper(args[0]), args[1:]
		if len(args) == 0 {
			return nil, errUsage
		}
	}
	urlStr := args[0]
	if strings.HasPrefix(urlStr, ":") {
		// shorthand for localhost.
		if strings.HasPrefix(urlStr, ":/") {
			urlStr = "http://localhost" + urlStr[1:]
		} else {
			urlStr = "http://localhost" + urlStr
		}
	}
	if !strings.HasPrefix(urlStr, "http:") && !strings.HasPrefix(urlStr, "https:") {
		urlStr = "http://" + urlStr
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %v", args[0], err)
	}
	if u.Host == "" {
		u.Host = "localhost"
	}
	p.url, args = u, args[1:]
	p.keyVals = make([]keyVal, len(args))
	for i, arg := range args {
		kv, err := parseKeyVal(arg)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q: %v", arg, err)
		}
		if isDataSendingSep(kv.sep) && p.method == "" {
			p.method = "POST"
		}
		p.keyVals[i] = kv
	}
	if p.method == "" {
		p.method = "GET"
	}
	return &p, nil
}

func isDataSendingSep(sep string) bool {
	sep = strings.TrimSuffix(sep, "@")
	return sep == ":=" || sep == "=" || sep == ""
}

func isMethod(s string) bool {
	for _, r := range s {
		if !('a' <= r && r <= 'z' ||
			'A' <= r && r <= 'Z') {
			return false
		}
	}
	return true
}

func (req *request) do(client *httpbakery.Client, stdin io.Reader) (*http.Response, error) {
	httpReq := &http.Request{
		URL:        req.url,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Method:     req.method,
		Header:     req.header,
	}
	if len(req.urlValues) > 0 {
		if httpReq.URL.RawQuery != "" {
			httpReq.URL.RawQuery += "&"
		}
		httpReq.URL.RawQuery += req.urlValues.Encode()
	}
	var body []byte
	switch {
	case len(req.form) > 0:
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		body = []byte(req.form.Encode())

	case len(req.jsonObj) > 0:
		data, err := json.Marshal(req.jsonObj)
		if err != nil {
			return nil, fmt.Errorf("cannot marshal JSON: %v", err)
		}
		body = data
	case httpReq.Method != "GET" && httpReq.Method != "HEAD" && stdin != nil:
		// No fields specified and it looks like we need a body.

		// TODO check if it's seekable or make a temp file.
		data, err := ioutil.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("error reading stdin: %v", err)
		}
		// TODO if we're expecting JSON, accept rjson too.
		body = data
	}
	httpReq.ContentLength = int64(len(body))
	httpReq.Body = ioutil.NopCloser(bytes.NewReader(body))

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cannot do HTTP request: %v", err)
	}
	return resp, nil
}

func showResponse(p *params, resp *http.Response, stdout io.Writer) error {
	if p.checkStatus && resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "warning: HTTP response code %s\n", resp.Status)
	}
	if p.headers {
		fmt.Fprintf(stdout, "%s %s\n", resp.Proto, resp.Status)
		printHeaders(stdout, resp.Header)
		fmt.Fprintf(stdout, "\n")
	}
	if !p.body {
		return nil
	}
	isJSONResp := false
	if ctype := resp.Header.Get("Content-Type"); ctype != "" {
		mediaType, _, err := mime.ParseMediaType(ctype)
		if err != nil {
			warningf("invalid content type %q in response", ctype)
		} else {
			isJSONResp = mediaType == "application/json"
		}
	}
	if !isJSONResp || p.raw {
		// TODO uncompress?
		io.Copy(stdout, resp.Body)
		return nil
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %v", err)
	}
	var indented bytes.Buffer
	if err := rjson.Indent(&indented, data, "", "\t"); err != nil {
		warningf("cannot pretty print JSON response: %v", err)
		stdout.Write(data)
		return nil
	}
	data = indented.Bytes()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	stdout.Write(data)
	return nil
}

func printHeaders(w io.Writer, h http.Header) {
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, attr := range h[key] {
			fmt.Fprintf(w, "%s: %s\n", key, attr)
		}
	}
}

func newClient(p *params) (*cookiejar.Jar, *httpbakery.Client, error) {
	client := httpbakery.NewClient()
	if p.agentFile != "" {
		v, err := readAgentsFile(p.agentFile)
		if err != nil {
			return nil, nil, errgo.Notef(err, "cannot read agents file")
		}
		if err := agent.SetUpAuth(client, v); err != nil {
			return nil, nil, errgo.Mask(err)
		}
	}
	client.AddInteractor(httpbakery.WebBrowserInteractor{})
	if p.insecure {
		rt := *http.DefaultTransport.(*http.Transport)
		rt.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
		client.Transport = &rt
	}

	if p.cookieFile == "" {
		return nil, client, nil
	}

	jar, err := cookiejar.New(&cookiejar.Options{
		Filename: p.cookieFile,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create cookie jar: %v", err)
	}
	client.Client.Jar = jar
	return jar, client, nil
}

var sepFuncs = map[string]func(req *request, p *params, key, val string) error{
	":":   (*request).httpHeader,
	"==":  (*request).urlParam,
	"=":   (*request).dataString,
	"=@":  (*request).dataStringFile,
	":=":  (*request).jsonOther,
	":=@": (*request).jsonOtherFile,
}

func (req *request) addKeyVal(p *params, kv keyVal) error {
	f := sepFuncs[kv.sep]
	if f == nil {
		return fmt.Errorf("key value type separator %q not yet recognized", kv.sep)
	}
	return f(req, p, kv.key, kv.val)
}

// separators holds all the possible key-pair separators, most ambiguous first.
var separators = []string{
	":=@", // raw JSON file
	":=",  // raw JSON value
	":",   // HTTP header
	"==",  // URL parameter
	"=@",  // data field from file.
	"=",   // data field.
	"@",   // form file field.
}

func parseKeyVal(s string) (keyVal, error) {
	keyBytes := make([]byte, 0, len(s))
	wasBackslash := false
	for i, r := range s {
		if r == '\\' && !wasBackslash {
			wasBackslash = true
			continue
		}
		if wasBackslash {
			keyBytes = append(keyBytes, string(r)...)
			wasBackslash = false
			continue
		}
		for _, sep := range separators {
			if !strings.HasPrefix(s[i:], sep) {
				continue
			}
			if len(keyBytes) == 0 {
				return keyVal{}, fmt.Errorf("empty key")
			}
			val := s[i+len(sep):]
			return keyVal{
				key: string(keyBytes),
				sep: sep,
				val: val,
			}, nil
		}
		keyBytes = append(keyBytes, string(r)...)
	}
	return keyVal{}, fmt.Errorf("no key-pair separator found")
}

// key:val
func (req *request) httpHeader(p *params, key, val string) error {
	req.header.Add(key, val)
	return nil
}

// key==val
func (req *request) urlParam(p *params, key, val string) error {
	req.urlValues.Add(key, val)
	return nil
}

// key=val
func (req *request) dataString(p *params, key, val string) error {
	if p.json {
		req.jsonObj[key] = val
	} else {
		req.form.Add(key, val)
	}
	return nil
}

// key=@val
func (req *request) dataStringFile(p *params, key, val string) error {
	data, err := ioutil.ReadFile(val)
	if err != nil {
		return err
	}
	return req.dataString(p, key, string(data))
}

// key:=val
func (req *request) jsonOther(p *params, key, val string) error {
	if !p.json {
		return fmt.Errorf("cannot specify non-string key unless --json is specified")
	}
	var m json.RawMessage
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return fmt.Errorf("invalid JSON in key %s: %v", key, err)
	}
	req.jsonObj[key] = &m
	return nil
}

// key:=@file
func (req *request) jsonOtherFile(p *params, key, val string) error {
	data, err := ioutil.ReadFile(val)
	if err != nil {
		return err
	}
	return req.jsonOther(p, key, string(data))
}

func fatalf(f string, a ...interface{}) {
	if strings.HasSuffix(f, "\n") {
		f = f[0 : len(f)-1]
	}
	fmt.Fprintf(os.Stderr, "http: %s\n", fmt.Sprintf(f, a...))
	os.Exit(1)
}

func warningf(f string, a ...interface{}) {
	if strings.HasSuffix(f, "\n") {
		f = f[0 : len(f)-1]
	}
	fmt.Fprintf(os.Stderr, "http: warning: %s\n", fmt.Sprintf(f, a...))
}

func isAllCaps(s string) bool {
	for _, r := range s {
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// readAgentsFile reads the file at path and returns an
// agent visitor suitable for performing agent authentication
// with the information found therein.
func readAgentsFile(path string) (*agent.AuthInfo, error) {
	var v agent.AuthInfo
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func defaultAgentFile() string {
	return filepath.Join(homeDir(), ".agents")
}

// homeDir returns the OS-specific home path as specified in the environment.
func homeDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("HOMEDRIVE"), os.Getenv("HOMEPATH"))
	}
	return os.Getenv("HOME")
}

type loggingTransport struct {
	transport http.RoundTripper
	printf    func(f string, a ...interface{})
}

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sendBody := replaceBody(&req.Body)

	t.printf("> %s %s\n", req.Method, req.URL)
	for _, line := range sortedHeader(req.Header) {
		t.printf("> %s: %s\n", line.name, line.val)
	}
	if len(sendBody) > 0 {
		t.printf("> body %q\n", sendBody)
	}
	t.printf(">\n")
	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		t.printf("< error %v\n", err)
		return resp, err
	}
	respBody := replaceBody(&resp.Body)
	t.printf("< %s\n", resp.Status)
	for _, line := range sortedHeader(resp.Header) {
		t.printf("< %s: %s\n", line.name, line.val)
	}
	if len(respBody) > 0 {
		t.printf("< body %q\n", respBody)
	}
	t.printf("<\n")
	return resp, nil
}

type headerLine struct {
	name string
	val  string
}

func sortedHeader(h http.Header) []headerLine {
	var lines []headerLine
	for name, vals := range h {
		for _, val := range vals {
			lines = append(lines, headerLine{name, val})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool {
		return lines[i].name < lines[j].name
	})
	return lines
}

func replaceBody(r *io.ReadCloser) []byte {
	if *r == nil {
		return nil
	}
	data, _ := ioutil.ReadAll(*r)
	(*r).Close()
	*r = ioutil.NopCloser(bytes.NewReader(data))
	return data
}
