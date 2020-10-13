// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package injectproxy

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"unicode"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"gopkg.in/square/go-jose.v2/json"
)

type routes struct {
	upstream             *url.URL
	handler              http.Handler
	label                string
	mux                  *http.ServeMux
	modifiers            map[string]func(*http.Response) error
	opaHTTPAuthzEndpoint string
}

func NewRoutes(upstream *url.URL, label string, opaHTTPAuthzEndpoint string) *routes {
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	r := &routes{
		upstream:             upstream,
		handler:              proxy,
		label:                label,
		opaHTTPAuthzEndpoint: opaHTTPAuthzEndpoint,
	}
	mux := http.NewServeMux()
	mux.Handle("/federate", enforceMethods(r.federate, "GET"))
	mux.Handle("/api/v1/query", enforceMethods(r.query, "GET", "POST"))
	mux.Handle("/api/v1/query_range", enforceMethods(r.query, "GET", "POST"))
	mux.Handle("/api/v1/alerts", enforceMethods(r.noop, "GET"))
	mux.Handle("/api/v1/rules", enforceMethods(r.noop, "GET"))
	mux.Handle("/api/v2/silences", enforceMethods(r.silences, "GET", "POST"))
	mux.Handle("/api/v2/silences/", enforceMethods(r.silences, "GET", "POST"))
	mux.Handle("/api/v2/silence/", enforceMethods(r.deleteSilence, "DELETE"))
	r.mux = mux
	r.modifiers = map[string]func(*http.Response) error{
		"/api/v1/rules":  modifyAPIResponse(r.filterRules),
		"/api/v1/alerts": modifyAPIResponse(r.filterAlerts),
	}
	proxy.ModifyResponse = r.ModifyResponse
	return r
}

func (r *routes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	queryString := req.URL.Query().Get("query")
	f := func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsNumber(c)
	}
	querySlice := strings.FieldsFunc(queryString, f)
	index := 0
	for i := range querySlice {
		if querySlice[i] == r.label {
			index = i + 1
			break
		}
	}
	lvalue := querySlice[index]
	// lvalue := req.URL.Query().Get(r.label)
	if lvalue == "" {
		http.Error(w, fmt.Sprintf("Bad request. The %q query parameter must be provided.", r.label), http.StatusBadRequest)
		return
	}

	// authorize request with opa
	httpStatus, httpStatusText, err := r.isUserAuthorized(req, lvalue)
	if httpStatus != http.StatusOK {
		http.Error(w, fmt.Sprintf("%v: %v", httpStatusText, err), httpStatus)
		return
	}

	req = req.WithContext(withLabelValue(req.Context(), lvalue))
	// Remove the proxy label from the query parameters.
	q := req.URL.Query()
	q.Del(r.label)
	req.URL.RawQuery = q.Encode()

	r.mux.ServeHTTP(w, req)
}

type opaPayload struct {
	Input struct {
		HTTP struct {
			Headers map[string]string `json:"headers"`
		} `json:"http"`
		Label map[string]string `json:"label"`
	} `json:"input"`
}

type opaResponse struct {
	Result struct {
		Allow bool `json:"allow"`
	} `json:"result"`
}

func (r *routes) isUserAuthorized(req *http.Request, val string) (int, string, error) {
	var opaPayload opaPayload
	var errorString string

	bearerToken := req.Header.Get("Authorization")
	label := make(map[string]string)
	label[r.label] = val
	headers := make(map[string]string)
	headers["authorization"] = "Bearer " + bearerToken
	opaPayload.Input.HTTP.Headers = headers
	opaPayload.Input.Label = label

	payload, err := json.Marshal(opaPayload)
	if err != nil {
		errorString = fmt.Sprintf("%v %v - failed to marshal OPA payload", http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return http.StatusInternalServerError, errorString, err
	}
	opaHTTPAuthzEndpoint := r.opaHTTPAuthzEndpoint
	opaReq, err := http.NewRequest("POST", opaHTTPAuthzEndpoint, bytes.NewBuffer(payload))
	if err != nil {
		errorString = fmt.Sprintf("%v %v - failed to create OPA HTTP request", http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return http.StatusInternalServerError, errorString, err
	}
	client := &http.Client{}
	resp, err := client.Do(opaReq)
	if err != nil {
		errorString = fmt.Sprintf("%v %v - failed to execute OPA HTTP request", http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return http.StatusInternalServerError, errorString, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errorString = fmt.Sprintf("%v %v - failed to read OPA response body", http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return http.StatusInternalServerError, errorString, err
	}

	opaResponse := &opaResponse{}
	err = json.Unmarshal(body, opaResponse)
	if err != nil {
		errorString = fmt.Sprintf("%v %v - failed to unmarshal to OPA response struct", http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return http.StatusInternalServerError, errorString, err
	}

	if opaResponse.Result.Allow {
		return http.StatusOK, http.StatusText(http.StatusOK), nil
	}

	errorString = fmt.Sprintf("%v %v - User not authorized", http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	return http.StatusUnauthorized, errorString, err
}

func (r *routes) ModifyResponse(resp *http.Response) error {
	m, found := r.modifiers[resp.Request.URL.Path]
	if !found {
		// Return the server's response unmodified.
		return nil
	}
	return m(resp)
}

func enforceMethods(h http.HandlerFunc, methods ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for _, m := range methods {
			if m == req.Method {
				h(w, req)
				return
			}
		}
		http.NotFound(w, req)
	})
}

type ctxKey int

const keyLabel ctxKey = iota

func mustLabelValue(ctx context.Context) string {
	label, ok := ctx.Value(keyLabel).(string)
	if !ok {
		panic(fmt.Sprintf("can't find the %q value in the context", keyLabel))
	}
	if label == "" {
		panic(fmt.Sprintf("empty %q value in the context", keyLabel))
	}

	return label
}

func withLabelValue(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, keyLabel, label)
}

func (r *routes) noop(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *routes) query(w http.ResponseWriter, req *http.Request) {
	expr, err := parser.ParseExpr(req.FormValue("query"))
	if err != nil {
		return
	}

	e := NewEnforcer([]*labels.Matcher{
		{
			Name:  r.label,
			Type:  labels.MatchEqual,
			Value: mustLabelValue(req.Context()),
		},
	}...)
	if err := e.EnforceNode(expr); err != nil {
		return
	}

	q := req.URL.Query()
	q.Set("query", expr.String())
	req.URL.RawQuery = q.Encode()

	r.handler.ServeHTTP(w, req)
}

func (r *routes) federate(w http.ResponseWriter, req *http.Request) {
	matcher := &labels.Matcher{
		Name:  r.label,
		Type:  labels.MatchEqual,
		Value: mustLabelValue(req.Context()),
	}

	q := req.URL.Query()
	q.Set("match[]", "{"+matcher.String()+"}")
	req.URL.RawQuery = q.Encode()

	r.handler.ServeHTTP(w, req)
}
