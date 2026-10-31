// Package actions runs a local MCP server that lets Grok act on the chat
// during one turn: react to the user's message and send files.
package actions

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxFiles = 10
	maxBodyBytes    = 1 << 20
	protocolVersion = "2025-11-25"
)

// File is a file Grok asked to send.
type File struct {
	Path    string
	Name    string
	Caption string
	Size    int64
}

// Turn is the context of one grok run. A nil React or CheckFile means the
// matching tool is not offered.
type Turn struct {
	React     func(ctx context.Context, emoji string) error
	CheckFile func(path string) (File, error)
	MaxFiles  int // 0 means 10

	mu      sync.Mutex
	reacted bool
	files   []File
}

// Reacted reports whether a reaction was sent. It is safe to call after the turn ends.
func (t *Turn) Reacted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reacted
}

// Files returns a copy of the files queued for sending. It is safe to call after the turn ends.
func (t *Turn) Files() []File {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]File(nil), t.files...)
}

func (t *Turn) maxFiles() int {
	if t.MaxFiles > 0 {
		return t.MaxFiles
	}
	return defaultMaxFiles
}

// ToolError is an error whose message is safe and useful to show to Grok.
// Any other error from a Turn callback is replaced by a fixed message, since
// platform errors can embed request URLs that contain secrets.
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

// modelMessage returns the text of a *ToolError, or fallback for any other error.
func modelMessage(err error, fallback string) string {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return toolErr.Msg
	}
	return fallback
}

// Server is the local MCP endpoint. Each grok run gets its own bearer token.
type Server struct {
	ln net.Listener

	mu    sync.Mutex
	turns map[string]*Turn
}

// Listen binds addr, which must be a loopback address.
func Listen(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{ln: ln, turns: map[string]*Turn{}}, nil
}

// Port returns the TCP port the server listens on.
func (s *Server) Port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

// Close stops listening. Serve does this itself when its context ends.
func (s *Server) Close() error {
	return s.ln.Close()
}

// Serve answers requests until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	stopped := make(chan struct{})
	defer func() { <-stopped }()
	serving := make(chan struct{})
	defer close(serving)
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if srv.Shutdown(shutdownCtx) != nil {
				srv.Close()
			}
		case <-serving:
		}
	}()
	if err := srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Begin registers a turn and returns the environment entry that hands its
// token to grok, plus a function that revokes the token. end is idempotent.
func (s *Server) Begin(t *Turn) (env string, end func()) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("actions: no randomness: " + err.Error())
	}
	token := hex.EncodeToString(raw[:])
	s.mu.Lock()
	s.turns[token] = t
	s.mu.Unlock()
	var once sync.Once
	return TokenEnv + "=" + token, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.turns, token)
			s.mu.Unlock()
		})
	}
}

// lookup finds the turn for a token, comparing in constant time.
func (s *Server) lookup(token string) *Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *Turn
	for known, t := range s.turns {
		if subtle.ConstantTimeCompare([]byte(known), []byte(token)) == 1 {
			found = t
		}
	}
	return found
}

// Handler returns the HTTP handler serving /mcp.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.serveMCP)
	return mux
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var turn *Turn
	if ok {
		turn = s.lookup(token)
	}
	if turn == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, http.StatusBadRequest, rpcResponse{Error: &rpcError{Code: codeParseError, Message: "parse error"}})
		return
	}
	// Notifications carry no id and get no reply.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := rpcResponse{ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = protocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "grokbot", "version": "1"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList(turn)}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			resp.Error = &rpcError{Code: codeInvalidParams, Message: "invalid params"}
			break
		}
		resp.Result = callTool(r.Context(), turn, params.Name, params.Arguments)
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found"}
	}
	writeRPC(w, http.StatusOK, resp)
}

func writeRPC(w http.ResponseWriter, status int, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func toolList(t *Turn) []map[string]any {
	tools := []map[string]any{}
	if t.React != nil {
		tools = append(tools, map[string]any{
			"name":        "react",
			"description": "React to the user's current message with one emoji.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"emoji": map[string]any{"type": "string", "description": "A single emoji."}},
				"required":             []string{"emoji"},
				"additionalProperties": false,
			},
		})
	}
	if t.CheckFile != nil {
		tools = append(tools, map[string]any{
			"name":        "send_file",
			"description": "Send a regular file from this machine to the user (not a directory; use list_dir to list directories). path must be absolute.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Absolute path of the file."},
					"caption": map[string]any{"type": "string", "description": "Optional caption."},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		})
	}
	return tools
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func callTool(ctx context.Context, t *Turn, name string, args json.RawMessage) map[string]any {
	switch {
	case name == "react" && t.React != nil:
		var in struct {
			Emoji string `json:"emoji"`
		}
		if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Emoji) == "" {
			return toolResult("emoji is required", true)
		}
		if err := t.React(ctx, in.Emoji); err != nil {
			return toolResult(modelMessage(err, "could not react"), true)
		}
		t.mu.Lock()
		t.reacted = true
		t.mu.Unlock()
		return toolResult("Reacted.", false)
	case name == "send_file" && t.CheckFile != nil:
		var in struct {
			Path    string `json:"path"`
			Caption string `json:"caption"`
		}
		if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
			return toolResult("path is required", true)
		}
		if t.fileCount() >= t.maxFiles() {
			return toolResult("too many files in this reply", true)
		}
		file, err := t.CheckFile(in.Path)
		if err != nil {
			return toolResult(modelMessage(err, "could not send file"), true)
		}
		file.Caption = in.Caption
		t.mu.Lock()
		defer t.mu.Unlock()
		// Another call may have used the last slot while CheckFile ran.
		if len(t.files) >= t.maxFiles() {
			return toolResult("too many files in this reply", true)
		}
		t.files = append(t.files, file)
		return toolResult("Queued "+file.Name+" for sending.", false)
	case name == "send_file":
		return toolResult("文件功能当前不可用。", true)
	default:
		return toolResult("unknown tool", true)
	}
}

func (t *Turn) fileCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.files)
}
