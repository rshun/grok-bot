package actions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolReply struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})
	return s, ts
}

// tokenOf strips the variable name from the env string Begin returns.
func tokenOf(t *testing.T, env string) string {
	t.Helper()
	token, ok := strings.CutPrefix(env, "GROKBOT_TURN_TOKEN=")
	if !ok || len(token) != 32 {
		t.Fatalf("unexpected env %q", env)
	}
	return token
}

func post(t *testing.T, ts *httptest.Server, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func rpc(t *testing.T, ts *httptest.Server, token, method, params string) rpcReply {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	resp, data := post(t, ts, token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d: %s", method, resp.StatusCode, data)
	}
	var reply rpcReply
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatalf("%s: bad json %q: %v", method, data, err)
	}
	return reply
}

func call(t *testing.T, ts *httptest.Server, token, name, args string) toolReply {
	t.Helper()
	reply := rpc(t, ts, token, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
	if reply.Error != nil {
		t.Fatalf("tools/call error: %+v", reply.Error)
	}
	var out toolReply
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func okFile(path string) (File, error) {
	return File{Path: path, Name: "a.txt", Size: 3}, nil
}

func okReact(context.Context, string) error { return nil }

func TestRejectsMissingAndWrongToken(t *testing.T) {
	s, ts := newTestServer(t)
	_, end := s.Begin(&Turn{React: okReact})
	defer end()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, token := range []string{"", "your_token_here"} {
		resp, _ := post(t, ts, token, body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
}

func TestTokenRejectedAfterEnd(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{React: okReact})
	token := tokenOf(t, env)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"react","arguments":{"emoji":"x"}}}`
	if resp, _ := post(t, ts, token, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("before end: status %d", resp.StatusCode)
	}
	end()
	end() // must be idempotent
	if resp, _ := post(t, ts, token, body); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after end: status %d, want 401", resp.StatusCode)
	}
}

func TestInitializeAndList(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{React: okReact, CheckFile: okFile})
	defer end()
	token := tokenOf(t, env)

	reply := rpc(t, ts, token, "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"grok","version":"1"}}`)
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(reply.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-11-25" || init.ServerInfo.Name != "grokbot" {
		t.Fatalf("bad initialize result: %+v", init)
	}
	if _, ok := init.Capabilities["tools"]; !ok {
		t.Fatalf("capabilities lack tools: %+v", init.Capabilities)
	}

	resp, data := post(t, ts, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted || len(data) != 0 {
		t.Fatalf("notification: status %d body %q", resp.StatusCode, data)
	}

	if names := listTools(t, ts, token); len(names) != 2 || names[0] != "react" || names[1] != "send_file" {
		t.Fatalf("tools = %v, want [react send_file]", names)
	}

	env2, end2 := s.Begin(&Turn{React: okReact})
	defer end2()
	if names := listTools(t, ts, tokenOf(t, env2)); len(names) != 1 || names[0] != "react" {
		t.Fatalf("tools = %v, want [react]", names)
	}
}

func listTools(t *testing.T, ts *httptest.Server, token string) []string {
	t.Helper()
	reply := rpc(t, ts, token, "tools/list", `{"_meta":{"progressToken":0}}`)
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range out.Tools {
		if tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %q lacks description or schema", tool.Name)
		}
		names = append(names, tool.Name)
	}
	return names
}

func TestTurnsAreIsolated(t *testing.T) {
	s, ts := newTestServer(t)
	var mu sync.Mutex
	var calls []string
	react := func(name string) func(context.Context, string) error {
		return func(_ context.Context, emoji string) error {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, name+emoji)
			return nil
		}
	}
	a, b := &Turn{React: react("A")}, &Turn{React: react("B")}
	envA, endA := s.Begin(a)
	defer endA()
	_, endB := s.Begin(b)
	defer endB()

	out := call(t, ts, tokenOf(t, envA), "react", `{"emoji":"x"}`)
	if out.IsError {
		t.Fatalf("unexpected error: %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "Ax" {
		t.Fatalf("calls = %v, want only A", calls)
	}
	if !a.Reacted() || b.Reacted() {
		t.Fatalf("Reacted: A=%v B=%v", a.Reacted(), b.Reacted())
	}
}

func TestReactErrors(t *testing.T) {
	s, ts := newTestServer(t)
	turn := &Turn{React: func(context.Context, string) error { return errors.New("no such emoji") }}
	env, end := s.Begin(turn)
	defer end()
	token := tokenOf(t, env)

	out := call(t, ts, token, "react", `{"emoji":"x"}`)
	if !out.IsError || out.Content[0].Text != "no such emoji" {
		t.Fatalf("got %+v", out)
	}
	if turn.Reacted() {
		t.Fatal("failed react must not count")
	}
	if out := call(t, ts, token, "react", `{}`); !out.IsError {
		t.Fatalf("missing emoji accepted: %+v", out)
	}
}

func TestSendFileQueuesAndLimits(t *testing.T) {
	s, ts := newTestServer(t)
	turn := &Turn{
		MaxFiles: 2,
		CheckFile: func(path string) (File, error) {
			if path == "/bad" {
				return File{}, errors.New("file is too large")
			}
			return File{Path: path, Name: "a.txt", Size: 3}, nil
		},
	}
	env, end := s.Begin(turn)
	defer end()
	token := tokenOf(t, env)

	out := call(t, ts, token, "send_file", `{"path":"/one","caption":"hi"}`)
	if out.IsError {
		t.Fatalf("unexpected error: %+v", out)
	}
	files := turn.Files()
	if len(files) != 1 || files[0].Path != "/one" || files[0].Caption != "hi" {
		t.Fatalf("files = %+v", files)
	}

	if out := call(t, ts, token, "send_file", `{"path":"/bad"}`); !out.IsError || out.Content[0].Text != "file is too large" {
		t.Fatalf("bad file: %+v", out)
	}
	if len(turn.Files()) != 1 {
		t.Fatal("failed file was queued")
	}

	if out := call(t, ts, token, "send_file", `{"path":"/two"}`); out.IsError {
		t.Fatalf("second file: %+v", out)
	}
	if out := call(t, ts, token, "send_file", `{"path":"/three"}`); !out.IsError {
		t.Fatalf("limit not enforced: %+v", out)
	}
	if len(turn.Files()) != 2 {
		t.Fatalf("files = %+v", turn.Files())
	}
}

func TestSendFileDefaultLimitIsTen(t *testing.T) {
	s, ts := newTestServer(t)
	turn := &Turn{CheckFile: okFile}
	env, end := s.Begin(turn)
	defer end()
	token := tokenOf(t, env)
	for i := 0; i < 10; i++ {
		if out := call(t, ts, token, "send_file", `{"path":"/f"}`); out.IsError {
			t.Fatalf("file %d: %+v", i, out)
		}
	}
	if out := call(t, ts, token, "send_file", `{"path":"/f"}`); !out.IsError {
		t.Fatalf("11th file accepted: %+v", out)
	}
}

func TestUnavailableToolIsError(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{React: okReact})
	defer end()
	if out := call(t, ts, tokenOf(t, env), "send_file", `{"path":"/f"}`); !out.IsError {
		t.Fatalf("send_file without CheckFile succeeded: %+v", out)
	}
}

func TestUnknownToolIsError(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{React: okReact})
	defer end()
	if out := call(t, ts, tokenOf(t, env), "nope", `{}`); !out.IsError {
		t.Fatalf("unknown tool succeeded: %+v", out)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{})
	defer end()
	token := tokenOf(t, env)
	for _, method := range []string{"server/discover", "resources/list"} {
		reply := rpc(t, ts, token, method, `{"_meta":{}}`)
		if reply.Error == nil || reply.Error.Code != -32601 {
			t.Fatalf("%s: got %+v, want -32601", method, reply.Error)
		}
	}
	if reply := rpc(t, ts, token, "ping", `{}`); reply.Error != nil {
		t.Fatalf("ping: %+v", reply.Error)
	}
}

func TestGetIs405(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{})
	defer end()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tokenOf(t, env))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", resp.StatusCode)
	}
}

func TestMalformedBodyIsParseError(t *testing.T) {
	s, ts := newTestServer(t)
	env, end := s.Begin(&Turn{})
	defer end()
	resp, data := post(t, ts, tokenOf(t, env), `{not json`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "-32700") {
		t.Fatalf("status %d body %s", resp.StatusCode, data)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if s.Port() == 0 {
		t.Fatal("no port")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	env, end := s.Begin(&Turn{})
	defer end()
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(s.Port())+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+tokenOf(t, env))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}
