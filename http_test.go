package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	stdtesting "testing"
	"time"

	flag "github.com/juju/gnuflag"
	"github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"golang.org/x/net/context"
	gc "gopkg.in/check.v1"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon-bakery.v2/bakery/identchecker"
	"gopkg.in/macaroon-bakery.v2/bakerytest"
	"gopkg.in/macaroon-bakery.v2/httpbakery"
)

type suite struct {
	testing.LoggingSuite
}

var _ = gc.Suite(&suite{})

func TestPackage(t *stdtesting.T) {
	gc.TestingT(t)
}

var testKeys = []struct {
	key       string
	expectKey string
	expectErr string
}{{
	key:       "x",
	expectKey: "x",
}, {
	key:       "",
	expectErr: "empty key",
}, {
	key:       "foo",
	expectKey: "foo",
}, {
	key:       `hello\:x`,
	expectKey: "hello:x",
}, {
	key:       `\=`,
	expectKey: "=",
}, {
	key:       `\\`,
	expectKey: `\`,
}, {
	key:       `\\\\`,
	expectKey: `\\`,
}, {
	key:       `\:\=`,
	expectKey: `:=`,
}}

var testVals = []string{
	"x",
	":",
	"",
	"x=y",
	`foo\`,
}

var testOps = []string{
	":",
	"==",
	"=",
	":=",
	"@",
	"=@",
	":=@",
}

func (*suite) TestParseArg(c *gc.C) {
	for _, testKey := range testKeys {
		for _, testOp := range testOps {
			for _, testVal := range testVals {
				c.Logf("test %q %q %q", testKey.key, testOp, testVal)
				s := testKey.key + testOp + testVal
				kv, err := parseKeyVal(s)
				if testKey.expectErr != "" {
					c.Assert(err, gc.ErrorMatches, testKey.expectErr)
					continue
				}
				c.Assert(err, gc.IsNil)
				c.Check(kv.key, gc.Equals, testKey.expectKey)
				c.Check(kv.sep, gc.Equals, testOp)
				c.Check(kv.val, gc.Equals, testVal)
			}
		}
	}
}

type newRequestTest struct {
	about         string
	args          []string
	expectRequest request
	expectError   string
}

var newRequestTests = []newRequestTest{{
	about:       "no arguments",
	expectError: errUsage.Error(),
}, {
	about:       "method only",
	args:        []string{"get"},
	expectError: errUsage.Error(),
}, {
	about: "url only",
	args:  []string{"http://foo.com/"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "/",
		},
	},
}, {
	about: "url with get method",
	args:  []string{"get", "http://foo.com/"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "/",
		},
	},
}, {
	about: "method with lower case",
	args:  []string{"GeT", "http://foo.com/"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "/",
		},
	},
}, {
	about: "put method",
	args:  []string{"put", "http://foo.com/"},
	expectRequest: request{
		method: "PUT",
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "/",
		},
	},
}, {
	about: "localhost default with port",
	args:  []string{":8080/foo"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "localhost:8080",
			Path:   "/foo",
		},
	},
}, {
	about: "localhost default without port",
	args:  []string{":/foo"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "localhost",
			Path:   "/foo",
		},
	},
}, {
	about: "localhost default with non-numeric port",
	args:  []string{":foo"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "localhost:foo",
			Path:   "",
		},
	},
}, {
	about: "host name without scheme",
	args:  []string{"foo.com"},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "",
		},
	},
}, {
	about: "headers and json values",
	args: []string{
		"--json",
		"foo.com",
		"h1:hval1",
		"h2:hval2",
		"u1==uval1",
		"u2==uval2.1",
		"u2==uval2.2",
		"j1=123",
		"j2:=123",
		"j3=",
		"j4:=[1,2,3]",
	},
	expectRequest: request{
		method: "POST",
		header: http.Header{
			"H1":           {"hval1"},
			"H2":           {"hval2"},
			"Content-Type": {"application/json"},
		},
		urlValues: url.Values{
			"u1": {"uval1"},
			"u2": {"uval2.1", "uval2.2"},
		},
		jsonObj: map[string]interface{}{
			"j1": "123",
			"j2": rawMessage("123"),
			"j3": "",
			"j4": rawMessage("[1,2,3]"),
		},
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "",
		},
	},
}, {
	about: "specific content type and json values",
	args: []string{
		"--json",
		"foo.com",
		"h1:hval1",
		"Content-Type:application/foobar",
		"j1=123",
	},
	expectRequest: request{
		method: "POST",
		header: http.Header{
			"H1":           {"hval1"},
			"Content-Type": {"application/foobar"},
		},
		jsonObj: map[string]interface{}{
			"j1": "123",
		},
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "",
		},
	},
}, {
	about: "headers and form values",
	args: []string{
		"foo.com",
		"j1=123",
		"j2=",
		"j2=another",
	},
	expectRequest: request{
		method: "POST",
		form: url.Values{
			"j1": {"123"},
			"j2": {"", "another"},
		},
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
			Path:   "",
		},
	},
}, {
	about: "method overriding default POST",
	args: []string{
		"put",
		"foo.com",
		"j1=123",
	},
	expectRequest: request{
		method: "PUT",
		form: url.Values{
			"j1": {"123"},
		},
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
		},
	},
}, {
	about: "basic auth",
	args: []string{
		"--auth", "username:password",
		"foo.com",
	},
	expectRequest: request{
		method: "GET",
		url: &url.URL{
			Scheme: "http",
			Host:   "foo.com",
		},
		header: http.Header{
			"Authorization": {"Basic dXNlcm5hbWU6cGFzc3dvcmQ="},
		},
	},
}}

func rawMessage(s string) *json.RawMessage {
	m := json.RawMessage(s)
	return &m
}

func (test *newRequestTest) run(c *gc.C, testIndex int) {
	c.Logf("test %d: %s", testIndex, test.about)
	fset := flag.NewFlagSet("http", flag.ContinueOnError)
	req, _, err := newRequest(fset, test.args)
	if test.expectError != "" {
		c.Assert(err, gc.ErrorMatches, test.expectError)
		return
	}
	c.Assert(err, gc.IsNil)
	if len(req.header) == 0 {
		req.header = nil
	}
	if len(req.urlValues) == 0 {
		req.urlValues = nil
	}
	if len(req.form) == 0 {
		req.form = nil
	}
	if len(req.jsonObj) == 0 {
		req.jsonObj = nil
	}
	c.Logf("url %s", req.url)
	c.Assert(req, jc.DeepEquals, &test.expectRequest)
}

func (*suite) TestNewRequest(c *gc.C) {
	for i, test := range newRequestTests {
		test.run(c, i)
	}
}

func (*suite) TestNewRequestWithFileVals(c *gc.C) {
	f, err := ioutil.TempFile("", "bhttp_test")
	c.Assert(err, gc.IsNil)
	defer os.Remove(f.Name())
	text := `{"x":true}`
	_, err = f.Write([]byte(text))
	c.Assert(err, gc.IsNil)
	f.Close()

	tests := []newRequestTest{{
		about: "form value in file",
		args: []string{
			"foo.com",
			"j1=@" + f.Name(),
		},
		expectRequest: request{
			method: "POST",
			form: url.Values{
				"j1": {text},
			},
			url: &url.URL{
				Scheme: "http",
				Host:   "foo.com",
			},
		},
	}, {
		about: "json data values in file",
		args: []string{
			"--json",
			"foo.com",
			"u1=@" + f.Name(),
			"u2:=@" + f.Name(),
		},
		expectRequest: request{
			method: "POST",
			jsonObj: map[string]interface{}{
				"u1": text,
				"u2": rawMessage(text),
			},
			url: &url.URL{
				Scheme: "http",
				Host:   "foo.com",
				Path:   "",
			},
			header: http.Header{
				"Content-Type": {"application/json"},
			},
		},
	}}
	for i, test := range tests {
		test.run(c, i)
	}
}

var requestDoTests = []struct {
	about                 string
	url                   string
	req                   request
	expectHTTPRequest     http.Request
	expectHTTPRequestBody string
	stdin                 string
}{{
	about: "get request with header",
	url:   "/foo",
	req: request{
		method: "GET",
		header: http.Header{
			"X-Something": {"foo"},
		},
	},
	expectHTTPRequest: http.Request{
		URL: &url.URL{
			Path: "/foo",
		},
		Header: http.Header{
			"X-Something": {"foo"},
		},
		Method: "GET",
	},
}, {
	about: "get request with url values",
	url:   "/foo",
	req: request{
		method: "GET",
		urlValues: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectHTTPRequest: http.Request{
		Method: "GET",
		URL: &url.URL{
			Path:     "/foo",
			RawQuery: "x=xval1&x=xval2&y=yval",
		},
		Form: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
}, {
	about: "get request with url values",
	url:   "/foo",
	req: request{
		method: "GET",
		urlValues: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectHTTPRequest: http.Request{
		Method: "GET",
		URL: &url.URL{
			Path:     "/foo",
			RawQuery: "x=xval1&x=xval2&y=yval",
		},
		Form: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
}, {
	about: "get request with url values, some explicitly set",
	url:   "/foo?z=zval",
	req: request{
		method: "GET",
		urlValues: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectHTTPRequest: http.Request{
		Method: "GET",
		URL: &url.URL{
			Path:     "/foo",
			RawQuery: "z=zval&x=xval1&x=xval2&y=yval",
		},
		Form: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
			"z": {"zval"},
		},
	},
}, {
	about: "post request with form values in body",
	url:   "/foo",
	req: request{
		method: "POST",
		form: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectHTTPRequest: http.Request{
		Method: "POST",
		URL: &url.URL{
			Path: "/foo",
		},
		Form: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
}, {
	about: "post request with JSON body",
	url:   "/foo",
	req: request{
		method: "POST",
		jsonObj: map[string]interface{}{
			"x": "hello",
		},
		header: http.Header{
			"Content-Type": {"application/json"},
		},
	},
	expectHTTPRequest: http.Request{
		URL: &url.URL{
			Path: "/foo",
		},
		Header: http.Header{
			"Content-Type": {"application/json"},
		},
		Method: "POST",
	},
	expectHTTPRequestBody: `{"x":"hello"}`,
}}

func (*suite) TestRequestDo(c *gc.C) {
	var h handler
	srv := httptest.NewServer(&h)
	for i, test := range requestDoTests {
		c.Logf("test %d: %s", i, test.about)
		client := httpbakery.NewClient()
		u, err := url.Parse(srv.URL + test.url)
		c.Assert(err, gc.IsNil)
		test.req.url = u
		if test.req.header == nil {
			test.req.header = make(http.Header)
		}
		resp, err := test.req.do(client, strings.NewReader(test.stdin))
		c.Assert(err, gc.IsNil)
		c.Assert(resp.StatusCode, gc.Equals, http.StatusOK)
		resp.Body.Close()

		// Don't do a DeepEquals on the expected request,
		// as it will contain all kinds of stuff that we aren't
		// that concerned with. Instead, test that the
		// data we've specified is there in the request.
		c.Assert(h.httpRequest.Method, gc.Equals, test.expectHTTPRequest.Method)
		for attr, vals := range test.expectHTTPRequest.Header {
			c.Assert(h.httpRequest.Header[attr], jc.DeepEquals, vals, gc.Commentf("attr %s", attr))
		}
		h.httpRequest.URL.Host = ""
		c.Assert(h.httpRequest.URL, jc.DeepEquals, test.expectHTTPRequest.URL)
		c.Assert(string(h.httpRequestBody), gc.Equals, test.expectHTTPRequestBody)
	}
}

func (*suite) TestMacaraq(c *gc.C) {
	checked := false
	d := bakerytest.NewDischarger(nil)
	d.Checker = httpbakery.ThirdPartyCaveatCheckerFunc(func(_ context.Context, _ *http.Request, info *bakery.ThirdPartyCaveatInfo, _ *httpbakery.DischargeToken) ([]checkers.Caveat, error) {
		if string(info.Condition) != "something" {
			return nil, fmt.Errorf("unexpected 3rd party cond")
		}
		checked = true
		return nil, nil
	})
	key, err := bakery.GenerateKey()
	c.Assert(err, gc.IsNil)
	b := identchecker.NewBakery(identchecker.BakeryParams{
		Location:       "here",
		Locator:        httpbakery.NewThirdPartyLocator(nil, nil),
		Key:            key,
		IdentityClient: idmClient{d.Location()},
	})
	oven := &httpbakery.Oven{
		Oven:        b.Oven,
		AuthnExpiry: time.Hour,
		AuthzExpiry: time.Hour,
	}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := context.Background()
		req.ParseForm()
		_, checkErr := b.Checker.Auth(httpbakery.RequestMacaroons(req)...).Allow(context.Background(), identchecker.LoginOp)
		if checkErr != nil {
			httpbakery.WriteError(ctx, w, oven.Error(ctx, req, checkErr))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data, err := json.Marshal(req.Form)
		c.Check(err, gc.IsNil)
		w.Write(data)
	}))

	fset := flag.NewFlagSet("http", flag.ContinueOnError)
	req, params, err := newRequest(fset, []string{
		svc.URL,
		"x=y",
	})
	c.Assert(err, gc.IsNil)
	client := httpbakery.NewClient()
	resp, err := req.do(client, nil)
	c.Assert(err, gc.IsNil)
	defer resp.Body.Close()
	c.Assert(resp.StatusCode, gc.Equals, http.StatusOK)
	c.Assert(checked, jc.IsTrue)

	var stdout bytes.Buffer
	err = showResponse(params, resp, &stdout)
	c.Assert(err, gc.IsNil)
	c.Assert(stdout.String(), gc.Equals, `{
	x: [
		"y"
	]
}
`)
}

type handler struct {
	httpRequest     http.Request
	httpRequestBody []byte

	next http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req.ParseForm()
	h.httpRequest = *req
	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		panic(err)
	}
	h.httpRequestBody = data
	req.Body = ioutil.NopCloser(bytes.NewReader(data))
	if h.next != nil {
		h.next.ServeHTTP(w, req)
	}
}

type idmClient struct {
	dischargerURL string
}

func (c idmClient) IdentityFromContext(ctxt context.Context) (identchecker.Identity, []checkers.Caveat, error) {
	return nil, []checkers.Caveat{{
		Location:  c.dischargerURL,
		Condition: "something",
	}}, nil
}

func (c idmClient) DeclaredIdentity(ctx context.Context, declared map[string]string) (identchecker.Identity, error) {
	return simpleIdentity(declared["username"]), nil
}

type simpleIdentity string

func (simpleIdentity) Domain() string {
	return ""
}

func (id simpleIdentity) Id() string {
	return string(id)
}
