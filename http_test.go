package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/macaroon-bakery.v1/bakery"
	"gopkg.in/macaroon-bakery.v1/bakery/checkers"
	"gopkg.in/macaroon-bakery.v1/bakerytest"
	"gopkg.in/macaroon-bakery.v1/httpbakery"
	flag "launchpad.net/gnuflag"
)

type suite struct{}

var _ = gc.Suite(&suite{})

func TestPackage(t *testing.T) {
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

var newContextTests = []struct {
	about         string
	args          []string
	expectContext context
	expectError   string
}{{
	about:       "no arguments",
	expectError: errUsage.Error(),
}, {
	about:       "method only",
	args:        []string{"get"},
	expectError: errUsage.Error(),
}, {
	about: "url only",
	args:  []string{"http://foo.com/"},
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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
	about: "headers and form values",
	args: []string{
		"foo.com",
		"j1=123",
		"j2=",
		"j2=another",
	},
	expectContext: context{
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
	expectContext: context{
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
	expectContext: context{
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

func (*suite) TestNewContext(c *gc.C) {
	for i, test := range newContextTests {
		c.Logf("test %d: %s", i, test.about)
		fset := flag.NewFlagSet("http", flag.ContinueOnError)
		ctxt, _, err := newContext(fset, test.args)
		if test.expectError != "" {
			c.Assert(err, gc.ErrorMatches, test.expectError)
			continue
		}
		c.Assert(err, gc.IsNil)
		if len(ctxt.header) == 0 {
			ctxt.header = nil
		}
		if len(ctxt.urlValues) == 0 {
			ctxt.urlValues = nil
		}
		if len(ctxt.form) == 0 {
			ctxt.form = nil
		}
		if len(ctxt.jsonObj) == 0 {
			ctxt.jsonObj = nil
		}
		c.Logf("url %s", ctxt.url)
		c.Assert(ctxt, jc.DeepEquals, &test.expectContext)
	}
}

var doRequestTests = []struct {
	about             string
	url               string
	ctxt              context
	expectRequest     http.Request
	expectRequestBody string
	stdin             string
}{{
	about: "get request with header",
	url:   "/foo",
	ctxt: context{
		method: "GET",
		header: http.Header{
			"X-Something": {"foo"},
		},
	},
	expectRequest: http.Request{
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
	ctxt: context{
		method: "GET",
		urlValues: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectRequest: http.Request{
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
	ctxt: context{
		method: "GET",
		urlValues: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectRequest: http.Request{
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
	ctxt: context{
		method: "GET",
		urlValues: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectRequest: http.Request{
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
	ctxt: context{
		method: "POST",
		form: url.Values{
			"x": {"xval1", "xval2"},
			"y": {"yval"},
		},
	},
	expectRequest: http.Request{
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
	ctxt: context{
		method: "POST",
		jsonObj: map[string]interface{}{
			"x": "hello",
		},
		header: http.Header{
			"Content-Type": {"application/json"},
		},
	},
	expectRequest: http.Request{
		URL: &url.URL{
			Path: "/foo",
		},
		Header: http.Header{
			"Content-Type": {"application/json"},
		},
		Method: "POST",
	},
	expectRequestBody: `{"x":"hello"}`,
}}

func (*suite) TestDoRequest(c *gc.C) {
	var h handler
	srv := httptest.NewServer(&h)
	for i, test := range doRequestTests {
		c.Logf("test %d: %s", i, test.about)
		client := httpbakery.NewClient()
		u, err := url.Parse(srv.URL + test.url)
		c.Assert(err, gc.IsNil)
		test.ctxt.url = u
		if test.ctxt.header == nil {
			test.ctxt.header = make(http.Header)
		}
		resp, err := test.ctxt.doRequest(client, strings.NewReader(test.stdin))
		c.Assert(err, gc.IsNil)
		c.Assert(resp.StatusCode, gc.Equals, http.StatusOK)
		resp.Body.Close()

		// Don't do a DeepEquals on the expected request,
		// as it will contain all kinds of stuff that we aren't
		// that concerned with. Instead, test that the
		// data we've specified is there in the request.
		c.Assert(h.request.Method, gc.Equals, test.expectRequest.Method)
		for attr, vals := range test.expectRequest.Header {
			c.Assert(h.request.Header[attr], jc.DeepEquals, vals, gc.Commentf("attr %s", attr))
		}
		h.request.URL.Host = ""
		c.Assert(h.request.URL, jc.DeepEquals, test.expectRequest.URL)
		c.Assert(string(h.requestBody), gc.Equals, test.expectRequestBody)
	}
}

func (*suite) TestMacaraq(c *gc.C) {
	checked := false
	d := bakerytest.NewDischarger(nil, func(_ *http.Request, cond, arg string) ([]checkers.Caveat, error) {
		if cond != "something" {
			return nil, fmt.Errorf("unexpected 3rd party cond")
		}
		checked = true
		return nil, nil
	})

	bsvc, err := bakery.NewService(bakery.NewServiceParams{
		Location: "here",
		Locator:  d,
	})
	c.Assert(err, gc.IsNil)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.ParseForm()
		_, checkErr := httpbakery.CheckRequest(bsvc, req, nil, checkers.New())
		if checkErr == nil {
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal(req.Form)
			c.Check(err, gc.IsNil)
			w.Write(data)
			return
		}
		m, err := bsvc.NewMacaroon("", nil, []checkers.Caveat{{
			Location:  d.Service.Location(),
			Condition: "something",
		}})
		c.Check(err, gc.IsNil)
		httpbakery.WriteDischargeRequiredError(w, m, "/", checkErr)
	}))

	fset := flag.NewFlagSet("http", flag.ContinueOnError)
	ctxt, params, err := newContext(fset, []string{
		svc.URL,
		"x=y",
	})
	c.Assert(err, gc.IsNil)
	client := httpbakery.NewClient()
	resp, err := ctxt.doRequest(client, nil)
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
	request     http.Request
	requestBody []byte

	next http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req.ParseForm()
	h.request = *req
	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		panic(err)
	}
	h.requestBody = data
	req.Body = ioutil.NopCloser(bytes.NewReader(data))
	if h.next != nil {
		h.next.ServeHTTP(w, req)
	}
}
