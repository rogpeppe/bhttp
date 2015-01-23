// Go clone of http(1)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"code.google.com/p/go.net/publicsuffix"
	"github.com/juju/persistent-cookiejar"
	"gopkg.in/macaroon-bakery.v0/httpbakery"
	flag "launchpad.net/gnuflag"
	"launchpad.net/rjson"
)

var flags struct {
	json       bool
	form       bool
	headers    bool
	body       bool
	rjson      bool
	raw        bool
	noBrowser  bool
	basicAuth  string
	cookieFile string

	// TODO auth, verify, proxy, file, timeout
}

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

func main() {
	flag.BoolVar(&flags.json, "j", false, "serialize  data  items  as a JSON object")
	flag.BoolVar(&flags.json, "json", false, "")

	flag.BoolVar(&flags.form, "f", false, "serialize data items as form values")
	flag.BoolVar(&flags.form, "form", false, "")

	flag.BoolVar(&flags.headers, "t", false, "print only the response headers")
	flag.BoolVar(&flags.headers, "headers", false, "")

	flag.BoolVar(&flags.body, "b", false, "print only the response body")
	flag.BoolVar(&flags.body, "body", false, "")

	flag.BoolVar(&flags.noBrowser, "B", false, "do not open URLs in web browser")
	flag.BoolVar(&flags.noBrowser, "no-browser", false, "")

	flag.BoolVar(&flags.raw, "raw", false, "print response body without any JSON post-processing")

	flag.StringVar(&flags.basicAuth, "a", "", "http basic auth (username:password)")
	flag.StringVar(&flags.basicAuth, "auth", "", "")

	flag.StringVar(&flags.cookieFile, "cookiefile", filepath.Join(os.Getenv("HOME"), ".go-cookies"), "file to store persistent cookies in")

	// TODO --file (multipart upload)
	// TODO --timeout
	// TODO --proxy
	// TODO (??) --verify

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, helpMessage)
		flag.PrintDefaults()
		os.Exit(2)
	}

	flag.Parse(true)
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
	}
	req := &http.Request{
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	if isAllCaps(args[0]) {
		req.Method, args = args[0], args[1:]
	}
	if len(args) == 0 {
		flag.Usage()
	}
	u, err := url.Parse(args[0])
	if err != nil {
		fatalf("invalid URL %q: %v", args[0], err)
	}
	args = args[1:]
	req.URL = u
	req.Host = req.URL.Host

	ctxt := &context{
		useJSON:   flags.json,
		method:    "GET",
		header:    make(http.Header),
		urlValues: make(url.Values),
		form:      make(url.Values),
		jsonObj:   make(map[string]interface{}),
	}
	for _, arg := range args {
		if err := ctxt.addArg(arg); err != nil {
			fatalf("%s", err)
		}
	}
	if err := ctxt.doRequest(u, req); err != nil {
		fatalf("%s", err)
	}
}

func (ctxt *context) doRequest(u *url.URL, req *http.Request) error {
	if req.Method == "" {
		req.Method = ctxt.method
	}
	if len(ctxt.urlValues) > 0 {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += ctxt.urlValues.Encode()
	}
	if ctxt.useJSON {
		maybeSetContentType(req, "application/json")
	}
	var body []byte
	req.Header = ctxt.header
	if flags.basicAuth != "" {
		req.Header["Authorization"] = []string{flags.basicAuth}
	}
	switch {
	case len(ctxt.form) > 0:
		maybeSetContentType(req, "application/x-www-form-urlencoded")
		body = []byte(ctxt.form.Encode())

	case len(ctxt.jsonObj) > 0:
		data, err := json.Marshal(ctxt.jsonObj)
		if err != nil {
			return fmt.Errorf("cannot marshal JSON: %v", err)
		}
		body = data
	case req.Method != "GET" && req.Method != "HEAD":
		// No fields specified and it looks like we need a body.

		// TODO check if it's seekable or make a temp file.
		data, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("error reading stdin: %v", err)
		}
		// TODO if we're expecting JSON, accept rjson too.
		body = data
	}
	getBody := httpbakery.SeekerBody(bytes.NewReader(body))

	jar, client, err := httpClient(flags.cookieFile)
	if err != nil {
		return fmt.Errorf("cannot make HTTP client: %v", err)
	}
	defer cookiejar.SaveToFile(jar, flags.cookieFile)
	resp, err := httpbakery.DoWithBody(client, req, getBody, visitWebPage)
	if err != nil {
		return fmt.Errorf("cannot do HTTP request: %v", err)
	}
	defer resp.Body.Close()
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
		io.Copy(os.Stdout, resp.Body)
		return nil
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %v", err)
	}
	var indented bytes.Buffer
	if err := rjson.Indent(&indented, data, "", "\t"); err != nil {
		warningf("cannot pretty print JSON response: %v", err)
		os.Stdout.Write(data)
		return nil
	}
	data = indented.Bytes()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	os.Stdout.Write(data)
	return nil
}

func visitWebPage(url *url.URL) error {
	fmt.Printf("please visit this URL:\n%s\n", url)
	return nil
}

func httpClient(cookieFile string) (*cookiejar.Jar, *http.Client, error) {
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		panic(err)
	}
	// TODO allow disabling of persistent cookies
	err = cookiejar.LoadFromFile(jar, cookieFile)
	if err != nil {
		return nil, nil, err
	}
	client := httpbakery.NewHTTPClient()
	client.Jar = jar
	return jar, client, nil
}

func maybeSetContentType(req *http.Request, t string) {
	if _, ok := req.Header["Content-Type"]; !ok {
		req.Header.Set("Content-Type", t)
	}
}

type context struct {
	useJSON bool

	method    string
	header    http.Header
	urlValues url.Values
	form      url.Values
	jsonObj   map[string]interface{}
}

var ops = map[string]func(ctxt *context, key, val string) error{
	":":  (*context).httpHeader,
	"==": (*context).urlParam,
	"=":  (*context).dataString,
	":=": (*context).jsonOther,
}

var argPat = regexp.MustCompile(`^((\\.)|[^:=@])(:|==|=|:=|@|=@|:=@)(.*)$`)

func (ctxt *context) addArg(s string) error {
	m := argPat.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("unrecognized key pair %q", s)
	}
	key, op, val := m[1], m[2], m[3]
	key = unquoteKey(key)
	f := ops[op]
	if f == nil {
		return fmt.Errorf("key value type %q not yet recognized", op)
	}
	return f(ctxt, key, val)
}

// key:val
func (ctxt *context) httpHeader(key, val string) error {
	ctxt.header.Add(key, val)
	return nil
}

// key==val
func (ctxt *context) urlParam(key, val string) error {
	ctxt.urlValues.Add(key, val)
	return nil
}

// key=val
func (ctxt *context) dataString(key, val string) error {
	ctxt.method = "POST"
	if ctxt.useJSON {
		ctxt.jsonObj[key] = val
	} else {
		ctxt.form.Add(key, val)
	}
	return nil
}

// key:=val
func (ctxt *context) jsonOther(key, val string) error {
	if !ctxt.useJSON {
		return fmt.Errorf("cannot specify non-string key unless --json is specified")
	}
	var m json.RawMessage
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return fmt.Errorf("invalid JSON in key %s: %v", key, err)
	}
	ctxt.jsonObj[key] = &m
	return nil
}

func unquoteKey(key string) string {
	nkey := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		if i < len(key)-1 && key[i] == '\\' {
			nkey = append(nkey, key[i+1])
			i++
		} else {
			nkey = append(nkey, key[i])
		}
	}
	return string(nkey)

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
