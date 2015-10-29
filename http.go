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
	"sort"
	"strings"
	"unicode"

	"github.com/juju/loggo"
	"github.com/juju/persistent-cookiejar"
	"github.com/rogpeppe/rjson"
	"gopkg.in/macaroon-bakery.v1/httpbakery"
	flag "launchpad.net/gnuflag"
)

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
      
      '@' Form file fields (only with --form, -f):
      
          cs@~/Documents/CV.pdf
      
      '=@' A data field like '=', but takes a file path and embeds its content:
      
           essay=@Documents/essay.txt
      
      ':=@' A raw JSON field like ':=', but takes a file path and embeds its content:
      
          package:=@./package.json
      
      You can use a backslash to escape a colliding separator in the field name:
      
          field-name-with\:colon=value
`

type params struct {
	json       bool
	form       bool
	headers    bool
	body       bool
	rjson      bool
	raw        bool
	debug      bool
	noBrowser  bool
	basicAuth  string
	cookieFile string
	useStdin   bool
	insecure   bool
	// TODO auth, verify, proxy, file, timeout

	url     *url.URL
	method  string
	keyVals []keyVal
}

type context struct {
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

type keyVal struct {
	key string
	sep string
	val string
}

func main() {
	fset := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	ctxt, p, err := newContext(fset, os.Args[1:])
	if err != nil {
		if err == errUsage {
			fset.Usage()
		} else {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		}
		os.Exit(2)
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
	resp, err := ctxt.doRequest(client, stdin)
	if err != nil {
		fatalf("%v", err)
	}
	defer resp.Body.Close()
	if err := showResponse(p, resp, os.Stdout); err != nil {
		fatalf("%v", err)
	}
}

func newContext(fset *flag.FlagSet, args []string) (*context, *params, error) {
	p, err := parseArgs(fset, args)
	if err != nil {
		return nil, nil, err
	}
	if p.debug {
		loggo.ConfigureLoggers("DEBUG")
	}
	ctxt := &context{
		url:       p.url,
		method:    p.method,
		header:    make(http.Header),
		urlValues: make(url.Values),
		form:      make(url.Values),
		jsonObj:   make(map[string]interface{}),
	}
	for _, kv := range p.keyVals {
		if err := ctxt.addKeyVal(p, kv); err != nil {
			return nil, nil, err
		}
	}
	if p.useStdin && (len(ctxt.form) > 0 || len(ctxt.jsonObj) > 0) {
		return nil, nil, errors.New("cannot read body from stdin when form or JSON body is specified")
	}
	if p.basicAuth != "" {
		ctxt.header.Set("Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(p.basicAuth)))
	}
	if p.json {
		ctxt.header.Set("Content-Type", "application/json")
	}
	return ctxt, p, nil
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

	fset.BoolVar(&p.debug, "debug", false, "print debugging messages")

	fset.BoolVar(&p.noBrowser, "W", false, "do not open macaroon-login URLs in web browser")
	fset.BoolVar(&p.noBrowser, "no-browser", false, "")

	fset.BoolVar(&p.raw, "raw", false, "print response body without any JSON post-processing")

	fset.StringVar(&p.basicAuth, "a", "", "http basic auth (username:password)")
	fset.StringVar(&p.basicAuth, "auth", "", "")

	fset.BoolVar(&p.insecure, "insecure", false, "skip HTTPS certificate checking")

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
		if kv.sep == "=" && p.method == "" {
			p.method = "POST"
		}
		p.keyVals[i] = kv
	}
	if p.method == "" {
		p.method = "GET"
	}
	return &p, nil
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

func (ctxt *context) doRequest(client *httpbakery.Client, stdin io.Reader) (*http.Response, error) {
	req := &http.Request{
		URL:        ctxt.url,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Method:     ctxt.method,
		Header:     ctxt.header,
	}
	if len(ctxt.urlValues) > 0 {
		if req.URL.RawQuery != "" {
			req.URL.RawQuery += "&"
		}
		req.URL.RawQuery += ctxt.urlValues.Encode()
	}
	var body []byte
	switch {
	case len(ctxt.form) > 0:
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		body = []byte(ctxt.form.Encode())

	case len(ctxt.jsonObj) > 0:
		data, err := json.Marshal(ctxt.jsonObj)
		if err != nil {
			return nil, fmt.Errorf("cannot marshal JSON: %v", err)
		}
		body = data
	case req.Method != "GET" && req.Method != "HEAD" && stdin != nil:
		// No fields specified and it looks like we need a body.

		// TODO check if it's seekable or make a temp file.
		data, err := ioutil.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("error reading stdin: %v", err)
		}
		// TODO if we're expecting JSON, accept rjson too.
		body = data
	}
	req.ContentLength = int64(len(body))

	resp, err := client.DoWithBody(req, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cannot do HTTP request: %v", err)
	}
	return resp, nil
}

func showResponse(p *params, resp *http.Response, stdout io.Writer) error {
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
	if !isJSONResp {
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
	client.VisitWebPage = httpbakery.OpenWebBrowser
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
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create cookie jar: %v", err)
	}
	client.Client.Jar = jar
	return jar, client, nil
}

var sepFuncs = map[string]func(ctxt *context, p *params, key, val string) error{
	":":  (*context).httpHeader,
	"==": (*context).urlParam,
	"=":  (*context).dataString,
	":=": (*context).jsonOther,
}

func (ctxt *context) addKeyVal(p *params, kv keyVal) error {
	f := sepFuncs[kv.sep]
	if f == nil {
		return fmt.Errorf("key value type separator %q not yet recognized", kv.sep)
	}
	return f(ctxt, p, kv.key, kv.val)
}

// separators holds all the possible key-pair separators, most ambiguous first.
var separators = []string{
	":=@",
	":=",
	":",
	"==",
	"=@",
	"=",
	"@",
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
			if strings.HasPrefix(s[i:], sep) {
				if len(keyBytes) == 0 {
					return keyVal{}, fmt.Errorf("empty key")
				}
				return keyVal{
					key: string(keyBytes),
					sep: sep,
					val: s[i+len(sep):],
				}, nil
			}
		}
		keyBytes = append(keyBytes, string(r)...)
	}
	return keyVal{}, fmt.Errorf("no key-pair separator found")
}

// key:val
func (ctxt *context) httpHeader(p *params, key, val string) error {
	ctxt.header.Add(key, val)
	return nil
}

// key==val
func (ctxt *context) urlParam(p *params, key, val string) error {
	ctxt.urlValues.Add(key, val)
	return nil
}

// key=val
func (ctxt *context) dataString(p *params, key, val string) error {
	if p.json {
		ctxt.jsonObj[key] = val
	} else {
		ctxt.form.Add(key, val)
	}
	return nil
}

// key:=val
func (ctxt *context) jsonOther(p *params, key, val string) error {
	if !p.json {
		return fmt.Errorf("cannot specify non-string key unless --json is specified")
	}
	var m json.RawMessage
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return fmt.Errorf("invalid JSON in key %s: %v", key, err)
	}
	ctxt.jsonObj[key] = &m
	return nil
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
